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

package repositories

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/wso2/agent-manager/agent-manager-service/models"
)

var (
	// ErrAgentPublicationNotFound means the agent is not published (in that environment).
	ErrAgentPublicationNotFound = errors.New("agent publication not found")
	// ErrAgentPublishedFromOtherEnvironment means the agent is already published from a different
	// environment; it must be unpublished there first.
	ErrAgentPublishedFromOtherEnvironment = errors.New("agent is published from another environment")
)

// AgentPublicationRepository stores which agents are listed in the Agent Portal. Every query is
// scoped by the caller's organization.
//
//go:generate moq -rm -fmt goimports -skip-ensure -pkg repomocks -out repomocks/agent_publication_repository_mock.go . AgentPublicationRepository:AgentPublicationRepositoryMock
type AgentPublicationRepository interface {
	// WithTx returns the repository bound to tx, so its writes commit or roll back with the caller's.
	WithTx(tx *gorm.DB) AgentPublicationRepository

	// Upsert creates the agent's publication, or updates it when it is published from the same
	// environment, keeping its ID and creation time. It returns ErrAgentPublishedFromOtherEnvironment,
	// and writes nothing, when the agent is published from a different environment. CORSOriginAdded
	// stays true once set, so a later update cannot forget an origin publishing added.
	Upsert(ctx context.Context, publication *models.AgentPublication) (*models.AgentPublication, error)

	// GetByAgent returns the agent's publication, whichever environment it is in.
	GetByAgent(ctx context.Context, ouID, projectName, agentName string) (*models.AgentPublication, error)
	// GetByID returns the publication the portal addresses by ID.
	GetByID(ctx context.Context, ouID string, id uuid.UUID) (*models.AgentPublication, error)
	// List returns one page of the organization's publications, newest update first, and the
	// number matching the filter.
	List(ctx context.Context, ouID string, filter models.AgentPublicationFilter) ([]models.AgentPublication, int64, error)
	// ListTags returns every tag used by the organization's publications, sorted.
	ListTags(ctx context.Context, ouID string) ([]string, error)

	// Delete removes the agent's publication in the environment and returns the removed row.
	// It returns ErrAgentPublicationNotFound when the agent is not published there.
	Delete(ctx context.Context, ouID, projectName, agentName string, environmentUUID uuid.UUID) (*models.AgentPublication, error)
	// DeleteForAgent removes a deleted agent's publication, if any.
	DeleteForAgent(ctx context.Context, ouID, projectName, agentName string) error
	// DeleteForEnvironment removes every publication in a deleted environment.
	DeleteForEnvironment(ctx context.Context, ouID string, environmentUUID uuid.UUID) error
}

type agentPublicationRepository struct {
	db *gorm.DB
}

// NewAgentPublicationRepository creates an AgentPublicationRepository.
func NewAgentPublicationRepository(db *gorm.DB) AgentPublicationRepository {
	return &agentPublicationRepository{db: db}
}

func (r *agentPublicationRepository) WithTx(tx *gorm.DB) AgentPublicationRepository {
	return &agentPublicationRepository{db: tx}
}

// upsertAgentPublication decides "same environment updates, other environment conflicts" in one
// statement: the WHERE on DO UPDATE skips the update for another environment, so RETURNING yields
// no row. Reading first would race with a concurrent publish.
const upsertAgentPublication = `
INSERT INTO agent_publications (
	ou_id, project_name, agent_name, environment_uuid, environment_name,
	display_name, description, icon_url, tags, cors_origin_added, published_by
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?::jsonb, ?, ?)
ON CONFLICT (ou_id, project_name, agent_name) DO UPDATE SET
	environment_name  = EXCLUDED.environment_name,
	display_name      = EXCLUDED.display_name,
	description       = EXCLUDED.description,
	icon_url          = EXCLUDED.icon_url,
	tags              = EXCLUDED.tags,
	cors_origin_added = agent_publications.cors_origin_added OR EXCLUDED.cors_origin_added,
	published_by      = EXCLUDED.published_by,
	updated_at        = NOW()
WHERE agent_publications.environment_uuid = EXCLUDED.environment_uuid
RETURNING *`

func (r *agentPublicationRepository) Upsert(ctx context.Context, p *models.AgentPublication) (*models.AgentPublication, error) {
	tags := p.Tags
	if tags == nil {
		tags = []string{}
	}
	var row models.AgentPublication
	result := r.db.WithContext(ctx).Raw(upsertAgentPublication,
		p.OUID, p.ProjectName, p.AgentName, p.EnvironmentUUID, p.EnvironmentName,
		p.DisplayName, p.Description, p.IconURL, toJSONArray(tags), p.CORSOriginAdded, p.PublishedBy,
	).Scan(&row)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, ErrAgentPublishedFromOtherEnvironment
	}
	return &row, nil
}

func (r *agentPublicationRepository) GetByAgent(ctx context.Context, ouID, projectName, agentName string) (*models.AgentPublication, error) {
	return r.first(ctx, "ou_id = ? AND project_name = ? AND agent_name = ?", ouID, projectName, agentName)
}

func (r *agentPublicationRepository) GetByID(ctx context.Context, ouID string, id uuid.UUID) (*models.AgentPublication, error) {
	return r.first(ctx, "ou_id = ? AND id = ?", ouID, id)
}

func (r *agentPublicationRepository) first(ctx context.Context, query string, args ...interface{}) (*models.AgentPublication, error) {
	var row models.AgentPublication
	err := r.db.WithContext(ctx).Where(query, args...).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrAgentPublicationNotFound
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

func (r *agentPublicationRepository) List(ctx context.Context, ouID string, filter models.AgentPublicationFilter) ([]models.AgentPublication, int64, error) {
	query := r.db.WithContext(ctx).Model(&models.AgentPublication{}).Where("ou_id = ?", ouID)
	if filter.Search != "" {
		search := "%" + escapeLikePattern(filter.Search) + "%"
		query = query.Where("(display_name ILIKE ? OR description ILIKE ?)", search, search)
	}
	if filter.Tag != "" {
		query = query.Where("tags @> ?::jsonb", toJSONArray([]string{filter.Tag}))
	}

	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	var rows []models.AgentPublication
	// id breaks ties so pages never overlap or skip rows.
	err := query.Order("updated_at DESC, id ASC").Limit(filter.Limit).Offset(filter.Offset).Find(&rows).Error
	if err != nil {
		return nil, 0, err
	}
	return rows, total, nil
}

func (r *agentPublicationRepository) ListTags(ctx context.Context, ouID string) ([]string, error) {
	tags := []string{}
	err := r.db.WithContext(ctx).Raw(`
		SELECT DISTINCT t.tag
		FROM agent_publications p
		CROSS JOIN LATERAL jsonb_array_elements_text(p.tags) AS t(tag)
		WHERE p.ou_id = ?
		ORDER BY t.tag`, ouID).Scan(&tags).Error
	if err != nil {
		return nil, err
	}
	return tags, nil
}

func (r *agentPublicationRepository) Delete(ctx context.Context, ouID, projectName, agentName string, environmentUUID uuid.UUID) (*models.AgentPublication, error) {
	var row models.AgentPublication
	result := r.db.WithContext(ctx).Clauses(clause.Returning{}).
		Where("ou_id = ? AND project_name = ? AND agent_name = ? AND environment_uuid = ?",
			ouID, projectName, agentName, environmentUUID).
		Delete(&row)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, ErrAgentPublicationNotFound
	}
	return &row, nil
}

func (r *agentPublicationRepository) DeleteForAgent(ctx context.Context, ouID, projectName, agentName string) error {
	return r.db.WithContext(ctx).
		Where("ou_id = ? AND project_name = ? AND agent_name = ?", ouID, projectName, agentName).
		Delete(&models.AgentPublication{}).Error
}

func (r *agentPublicationRepository) DeleteForEnvironment(ctx context.Context, ouID string, environmentUUID uuid.UUID) error {
	return deleteForEnvironment[models.AgentPublication](ctx, r.db, ouID, environmentUUID)
}
