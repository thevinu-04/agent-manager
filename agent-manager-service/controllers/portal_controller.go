// Copyright (c) 2026, WSO2 LLC. (https://www.wso2.com).
//
// WSO2 LLC. licenses this file to you under the Apache License,
// Version 2.0 (the "License"); you may not use this file except
// in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package controllers

import (
	"errors"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/wso2/agent-manager/agent-manager-service/middleware"
	"github.com/wso2/agent-manager/agent-manager-service/middleware/jwtassertion"
	"github.com/wso2/agent-manager/agent-manager-service/middleware/logger"
	"github.com/wso2/agent-manager/agent-manager-service/models"
	"github.com/wso2/agent-manager/agent-manager-service/services"
	"github.com/wso2/agent-manager/agent-manager-service/spec"
	"github.com/wso2/agent-manager/agent-manager-service/utils"
)

// PathParamPublicationID names a published agent in the portal API.
const PathParamPublicationID = "publicationId"

// PortalController serves the Agent Portal catalog and chat keys.
type PortalController interface {
	ListPortalAgents(w http.ResponseWriter, r *http.Request)
	GetPortalAgent(w http.ResponseWriter, r *http.Request)
	IssuePortalChatKey(w http.ResponseWriter, r *http.Request)
}

type portalController struct {
	portalService services.PortalService
}

// NewPortalController creates a PortalController.
func NewPortalController(portalService services.PortalService) PortalController {
	return &portalController{portalService: portalService}
}

func (c *portalController) ListPortalAgents(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	limit, ok := queryInt(w, query.Get("limit"), "limit", 1, 100)
	if !ok {
		return
	}
	offset, ok := queryInt(w, query.Get("offset"), "offset", 0, -1)
	if !ok {
		return
	}
	page, err := c.portalService.ListAgents(r.Context(), middleware.OUIDFromRequest(r), models.AgentPublicationFilter{
		Search: query.Get("search"),
		Tag:    query.Get("tag"),
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		writePortalError(w, r, err, "Failed to list published agents")
		return
	}
	agents := make([]spec.PortalAgent, 0, len(page.Agents))
	for i := range page.Agents {
		agents = append(agents, toSpecPortalAgent(&page.Agents[i]))
	}
	tags := page.Tags
	if tags == nil {
		tags = []string{}
	}
	// The spec's counts are int32; a catalog never comes near that, but clamp rather than wrap.
	total := int32(min(page.Total, math.MaxInt32))                                 //nolint:gosec // clamped on this line
	limit32, offset32 := int32(page.Limit), int32(min(page.Offset, math.MaxInt32)) //nolint:gosec // limit ≤ 100, offset clamped
	utils.WriteSuccessResponse(w, http.StatusOK, spec.NewPortalAgentListResponse(agents, tags, total, limit32, offset32))
}

func (c *portalController) GetPortalAgent(w http.ResponseWriter, r *http.Request) {
	id, ok := publicationIDOf(w, r)
	if !ok {
		return
	}
	agent, err := c.portalService.GetAgent(r.Context(), middleware.OUIDFromRequest(r), id)
	if err != nil {
		writePortalError(w, r, err, "Failed to get the published agent")
		return
	}
	utils.WriteSuccessResponse(w, http.StatusOK, toSpecPortalAgent(agent))
}

func (c *portalController) IssuePortalChatKey(w http.ResponseWriter, r *http.Request) {
	id, ok := publicationIDOf(w, r)
	if !ok {
		return
	}
	key, err := c.portalService.IssueChatKey(r.Context(), middleware.OUIDFromRequest(r), id,
		jwtassertion.CallerSubject(r.Context()))
	if err != nil {
		writePortalError(w, r, err, "Failed to issue a chat key")
		return
	}
	expiresAt, err := time.Parse(time.RFC3339, key.ExpiresAt)
	if err != nil {
		writePortalError(w, r, err, "Failed to issue a chat key")
		return
	}
	utils.WriteSuccessResponse(w, http.StatusOK,
		spec.NewPortalChatKeyResponse(key.APIKey, expiresAt, key.GatewayConnected, key.ChatURL))
}

func toSpecPortalAgent(a *models.PortalAgent) spec.PortalAgent {
	p := a.Publication
	tags := p.Tags
	if tags == nil {
		tags = []string{}
	}
	out := spec.NewPortalAgent(p.ID.String(), p.DisplayName, p.Description, tags, p.UpdatedAt, a.Available)
	if p.IconURL != "" {
		out.SetIconUrl(p.IconURL)
	}
	return *out
}

// publicationIDOf parses the path's publication ID. A malformed one names no published agent: 404.
func publicationIDOf(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue(PathParamPublicationID))
	if err != nil {
		utils.WriteErrorResponse(w, http.StatusNotFound, utils.ErrPortalAgentNotFound.Error())
		return uuid.Nil, false
	}
	return id, true
}

// queryInt parses an optional integer query parameter within [lo, hi] (hi < 0: no upper bound),
// writing a 400 when it is malformed. An absent parameter is 0, for the service's default.
func queryInt(w http.ResponseWriter, raw, name string, lo, hi int) (int, bool) {
	if raw == "" {
		return 0, true
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < lo || (hi >= 0 && v > hi) {
		msg := name + " must be an integer of at least " + strconv.Itoa(lo)
		if hi >= 0 {
			msg += " and at most " + strconv.Itoa(hi)
		}
		utils.WriteErrorResponse(w, http.StatusBadRequest, msg)
		return 0, false
	}
	return v, true
}

// writePortalError maps the portal sentinels, then defers to handleCommonErrors.
func writePortalError(w http.ResponseWriter, r *http.Request, err error, fallback string) {
	logger.GetLogger(r.Context()).Warn("Agent Portal request failed", "path", r.URL.Path, "error", err)
	switch {
	case errors.Is(err, utils.ErrPortalAgentNotFound):
		utils.WriteErrorResponse(w, http.StatusNotFound, err.Error())
	case errors.Is(err, utils.ErrPortalAgentUnavailable):
		utils.WriteErrorResponse(w, http.StatusConflict, "This agent is temporarily unavailable. Contact its owner.")
	case errors.Is(err, utils.ErrGatewayNotFound):
		// The spec's 503: the key has nowhere to go until a gateway serves the environment.
		utils.WriteErrorResponse(w, http.StatusServiceUnavailable, "No gateway serves this agent right now. Try again later.")
	default:
		handleCommonErrors(w, err, fallback)
	}
}
