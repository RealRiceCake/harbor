// Copyright Project Harbor Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package registry

import (
	"github.com/goharbor/harbor/src/api/artifact"
	"github.com/goharbor/harbor/src/api/event"
	"github.com/goharbor/harbor/src/api/repository"
	"github.com/goharbor/harbor/src/common/utils/log"
	"github.com/goharbor/harbor/src/internal"
	ierror "github.com/goharbor/harbor/src/internal/error"
	evt "github.com/goharbor/harbor/src/pkg/notifier/event"
	serror "github.com/goharbor/harbor/src/server/error"
	"github.com/goharbor/harbor/src/server/router"
	"github.com/opencontainers/go-digest"
	"net/http"
	"strings"
)

// make sure the artifact exist before proxying the request to the backend registry
func getManifest(w http.ResponseWriter, req *http.Request) {
	repository := router.Param(req.Context(), ":splat")
	reference := router.Param(req.Context(), ":reference")
	artifact, err := artifact.Ctl.GetByReference(req.Context(), repository, reference, nil)
	if err != nil {
		serror.SendError(w, err)
		return
	}

	// the reference is tag, replace it with digest
	if _, err = digest.Parse(reference); err != nil {
		req = req.Clone(req.Context())
		req.URL.Path = strings.TrimSuffix(req.URL.Path, reference) + artifact.Digest
		req.URL.RawPath = req.URL.EscapedPath()
	}

	recorder := internal.NewResponseRecorder(w)
	proxy.ServeHTTP(recorder, req)
	// fire event
	if recorder.Success() {
		// TODO don't fire event for the pulling from replication
		e := &event.PullArtifactEventMetadata{
			Ctx:      req.Context(),
			Artifact: &artifact.Artifact,
		}
		// TODO provide a util function to determine whether the reference is tag or not
		// the reference is tag
		if _, err = digest.Parse(reference); err != nil {
			e.Tag = reference
		}
		evt.BuildAndPublish(e)
	}
}

// just delete the artifact from database
func deleteManifest(w http.ResponseWriter, req *http.Request) {
	repository := router.Param(req.Context(), ":splat")
	reference := router.Param(req.Context(), ":reference")
	// v2 doesn't support delete by tag
	// add parse digest here is to return ErrDigestInvalidFormat before GetByReference throws an NOT_FOUND(404)
	// Do not add the logic into GetByReference as it's a shared method for PUT/GET/DELETE/Internal call,
	// and NOT_FOUND satisfy PUT/GET/Internal call.
	if _, err := digest.Parse(reference); err != nil {
		switch err {
		case digest.ErrDigestInvalidFormat:
			serror.SendError(w, ierror.New(nil).WithCode(ierror.DIGESTINVALID).
				WithMessage(digest.ErrDigestInvalidFormat.Error()))
			return
		}
	}
	art, err := artifact.Ctl.GetByReference(req.Context(), repository, reference, nil)
	if err != nil {
		serror.SendError(w, err)
		return
	}
	if err = artifact.Ctl.Delete(req.Context(), art.ID); err != nil {
		serror.SendError(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func putManifest(w http.ResponseWriter, req *http.Request) {
	repo := router.Param(req.Context(), ":splat")
	reference := router.Param(req.Context(), ":reference")

	// make sure the repository exist before pushing the manifest
	_, _, err := repository.Ctl.Ensure(req.Context(), repo)
	if err != nil {
		serror.SendError(w, err)
		return
	}

	buffer := internal.NewResponseBuffer(w)
	// proxy the req to the backend docker registry
	proxy.ServeHTTP(buffer, req)
	if !buffer.Success() {
		if _, err := buffer.Flush(); err != nil {
			log.Errorf("failed to flush: %v", err)
		}
		return
	}

	// When got the response from the backend docker registry, the manifest and
	// tag are both ready, so we don't need to handle the issue anymore:
	// https://github.com/docker/distribution/issues/2625

	var tags []string
	dgt := reference
	// the reference is tag, get the digest from the response header
	if _, err = digest.Parse(reference); err != nil {
		dgt = buffer.Header().Get("Docker-Content-Digest")
		tags = append(tags, reference)
	}

	_, _, err = artifact.Ctl.Ensure(req.Context(), repo, dgt, tags...)
	if err != nil {
		serror.SendError(w, err)
		return
	}

	// flush the origin response from the docker registry to the underlying response writer
	if _, err := buffer.Flush(); err != nil {
		log.Errorf("failed to flush: %v", err)
	}
}
