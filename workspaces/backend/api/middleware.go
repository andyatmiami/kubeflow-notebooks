/*
Copyright 2024.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package api

import (
	"fmt"
	"net/http"

	"github.com/julienschmidt/httprouter"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/kubeflow/notebooks/workspaces/backend/internal/helper"
)

func (a *App) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				w.Header().Set("Connection", "close")
				a.serverErrorResponse(w, r, fmt.Errorf("%s", err))
			}
		}()

		next.ServeHTTP(w, r)
	})
}

func (a *App) enableCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// TODO(ederign) restrict CORS to a much smaller set of trusted origins.
		// TODO(ederign) deal with preflight requests
		w.Header().Set("Access-Control-Allow-Origin", "*")

		next.ServeHTTP(w, r)
	})
}

// validatePathParams is a middleware that validates path parameters - currently only namespace and resource name parameters are validated
func (a *App) validatePathParams(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Get the path parameters from the request context
		params := httprouter.ParamsFromContext(r.Context())
		if params == nil {
			next.ServeHTTP(w, r)
			return
		}

		var valErrs field.ErrorList
		for _, param := range params {
			// Only validate namespace and resource name parameters
			if param.Key == NamespacePathParam || param.Key == ResourceNamePathParam {
				valErrs = append(valErrs, helper.ValidateFieldIsDNS1123Subdomain(field.NewPath(param.Key), param.Value)...)
			}
		}

		if len(valErrs) > 0 {
			a.failedValidationResponse(w, r, errMsgPathParamsInvalid, valErrs, nil)
			return
		}

		next.ServeHTTP(w, r)
	})
}
