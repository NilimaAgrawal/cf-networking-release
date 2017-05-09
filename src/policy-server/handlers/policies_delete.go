package handlers

import (
	"errors"
	"io/ioutil"
	"net/http"
	"policy-server/models"
	"policy-server/uaa_client"

	"code.cloudfoundry.org/go-db-helpers/marshal"
	"code.cloudfoundry.org/lager"
)

type PoliciesDelete struct {
	Logger        lager.Logger
	Unmarshaler   marshal.Unmarshaler
	Store         store
	Validator     validator
	PolicyGuard   policyGuard
	ErrorResponse errorResponse
}

func (h *PoliciesDelete) ServeHTTP(w http.ResponseWriter, req *http.Request, tokenData uaa_client.CheckTokenResponse) {
	bodyBytes, err := ioutil.ReadAll(req.Body)
	if err != nil {
		h.ErrorResponse.BadRequest(w, err, "policies-delete", "invalid request body")
		return
	}

	var payload struct {
		Policies []models.Policy `json:"policies"`
	}
	err = h.Unmarshaler.Unmarshal(bodyBytes, &payload)
	if err != nil {
		h.ErrorResponse.BadRequest(w, err, "policies-delete", "invalid values passed to API")
		return
	}

	err = h.Validator.ValidatePolicies(payload.Policies)
	if err != nil {
		h.ErrorResponse.BadRequest(w, err, "policies-delete", err.Error())
		return
	}

	authorized, err := h.PolicyGuard.CheckAccess(payload.Policies, tokenData)
	if err != nil {
		h.ErrorResponse.InternalServerError(w, err, "policies-delete", "check access failed")
		return
	}
	if !authorized {
		err := errors.New("one or more applications cannot be found or accessed")
		h.ErrorResponse.Forbidden(w, err, "policies-delete", err.Error())
		return
	}

	err = h.Store.Delete(payload.Policies)
	if err != nil {
		h.ErrorResponse.InternalServerError(w, err, "policies-delete", "database delete failed")
		return
	}

	h.Logger.Info("policy-delete", lager.Data{"policies": payload.Policies, "userName": tokenData.UserName})
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{}`))
	return
}
