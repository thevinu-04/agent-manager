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
	"encoding/json"
	"errors"
	"net/http"

	"github.com/wso2/agent-manager/agent-manager-service/middleware/logger"
	"github.com/wso2/agent-manager/agent-manager-service/models"
	"github.com/wso2/agent-manager/agent-manager-service/services"
	"github.com/wso2/agent-manager/agent-manager-service/spec"
	"github.com/wso2/agent-manager/agent-manager-service/utils"
)

// AgentPublicationController publishes agents to the Agent Portal.
type AgentPublicationController interface {
	GetAgentPublication(w http.ResponseWriter, r *http.Request)
	PublishAgent(w http.ResponseWriter, r *http.Request)
	UnpublishAgent(w http.ResponseWriter, r *http.Request)
}

type agentPublicationController struct {
	publicationService services.AgentPublicationService
}

// NewAgentPublicationController creates an AgentPublicationController.
func NewAgentPublicationController(publicationService services.AgentPublicationService) AgentPublicationController {
	return &agentPublicationController{publicationService: publicationService}
}

func (c *agentPublicationController) GetAgentPublication(w http.ResponseWriter, r *http.Request) {
	ouID, projName, agentName, envName := agentCardPath(r)
	status, err := c.publicationService.GetPublication(r.Context(), ouID, projName, agentName, envName)
	if err != nil {
		writeAgentPublicationError(w, r, err, "Failed to get the agent's publication")
		return
	}
	resp := spec.NewAgentPublicationStatusResponse(status.Publication != nil, toSpecEligibility(status))
	if status.Publication != nil {
		resp.SetPublication(toSpecAgentPublication(status.Publication))
	}
	utils.WriteSuccessResponse(w, http.StatusOK, resp)
}

func (c *agentPublicationController) PublishAgent(w http.ResponseWriter, r *http.Request) {
	ouID, projName, agentName, envName := agentCardPath(r)
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req spec.PublishAgentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.WriteErrorResponse(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	pub, err := c.publicationService.Publish(r.Context(), ouID, projName, agentName, envName, models.AgentPublicationRequest{
		DisplayName: req.DisplayName,
		Description: req.GetDescription(),
		IconURL:     req.GetIconUrl(),
		Tags:        req.Tags,
	})
	if err != nil {
		writeAgentPublicationError(w, r, err, "Failed to publish the agent")
		return
	}
	utils.WriteSuccessResponse(w, http.StatusOK, toSpecAgentPublication(pub))
}

func (c *agentPublicationController) UnpublishAgent(w http.ResponseWriter, r *http.Request) {
	ouID, projName, agentName, envName := agentCardPath(r)
	if err := c.publicationService.Unpublish(r.Context(), ouID, projName, agentName, envName); err != nil {
		writeAgentPublicationError(w, r, err, "Failed to unpublish the agent")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func toSpecAgentPublication(p *models.AgentPublication) spec.AgentPublication {
	tags := p.Tags
	if tags == nil {
		tags = []string{}
	}
	out := spec.NewAgentPublication(p.ID.String(), p.ProjectName, p.AgentName, p.EnvironmentName,
		p.DisplayName, p.Description, tags, p.PublishedBy, p.CreatedAt, p.UpdatedAt)
	if p.IconURL != "" {
		out.SetIconUrl(p.IconURL)
	}
	return *out
}

func toSpecEligibility(status *models.AgentPublicationStatus) spec.AgentPublicationEligibility {
	checks := make([]spec.AgentPublicationEligibilityCheck, 0, len(status.Checks))
	for _, c := range status.Checks {
		check := spec.NewAgentPublicationEligibilityCheck(c.ID, c.Passed)
		if c.Reason != "" {
			check.SetReason(c.Reason)
		}
		checks = append(checks, *check)
	}
	return *spec.NewAgentPublicationEligibility(status.Eligible(), checks)
}

// writeAgentPublicationError maps the publication sentinels, then defers to handleCommonErrors
// (validation errors, not-found, the tier 403 and the audit 503).
func writeAgentPublicationError(w http.ResponseWriter, r *http.Request, err error, fallback string) {
	logger.GetLogger(r.Context()).Warn("Agent publication request failed", "path", r.URL.Path, "error", err)
	switch {
	case errors.Is(err, utils.ErrAgentNotPublished):
		utils.WriteErrorResponse(w, http.StatusNotFound, err.Error())
	case errors.Is(err, utils.ErrAgentPublishedElsewhere):
		// The console shows the message, so it carries the environment and the way out.
		utils.WriteErrorResponse(w, http.StatusConflict, err.Error())
	default:
		handleCommonErrors(w, err, fallback)
	}
}
