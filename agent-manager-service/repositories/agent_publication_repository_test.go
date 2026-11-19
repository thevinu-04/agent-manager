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

//go:build integration

package repositories

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wso2/agent-manager/agent-manager-service/db"
	"github.com/wso2/agent-manager/agent-manager-service/models"
)

// newTestPublication returns a publication in a fresh org, so tests never see each other's rows.
func newTestPublication(t *testing.T, repo AgentPublicationRepository, agentName string) *models.AgentPublication {
	t.Helper()
	p := &models.AgentPublication{
		OUID:            "ou-" + uuid.New().String()[:8],
		ProjectName:     "people",
		AgentName:       agentName,
		EnvironmentUUID: uuid.New(),
		EnvironmentName: "Production",
		DisplayName:     "HR Helper",
		Description:     "Answers leave and benefits questions",
		Tags:            []string{"HR"},
		PublishedBy:     "user-1",
	}
	t.Cleanup(func() {
		_ = repo.DeleteForAgent(context.Background(), p.OUID, p.ProjectName, p.AgentName)
	})
	return p
}

func TestAgentPublicationUpsertCreatesThenUpdatesInTheSameEnvironment(t *testing.T) {
	repo := NewAgentPublicationRepository(db.GetDB())
	ctx := context.Background()
	p := newTestPublication(t, repo, "hr-helper")

	created, err := repo.Upsert(ctx, p)
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, created.ID)
	assert.Equal(t, []string{"HR"}, created.Tags)

	update := *p
	update.DisplayName, update.Tags, update.PublishedBy = "HR Assistant", []string{"HR", "Benefits"}, "user-2"
	updated, err := repo.Upsert(ctx, &update)
	require.NoError(t, err)
	assert.Equal(t, created.ID, updated.ID, "republishing keeps the publication ID")
	assert.Equal(t, created.CreatedAt, updated.CreatedAt)
	assert.Equal(t, "HR Assistant", updated.DisplayName)
	assert.Equal(t, []string{"HR", "Benefits"}, updated.Tags)
	assert.Equal(t, "user-2", updated.PublishedBy, "published_by is the last publisher")

	got, err := repo.GetByAgent(ctx, p.OUID, p.ProjectName, p.AgentName)
	require.NoError(t, err)
	assert.Equal(t, "HR Assistant", got.DisplayName)
}

func TestAgentPublicationUpsertRejectsAnotherEnvironmentAndKeepsTheRow(t *testing.T) {
	repo := NewAgentPublicationRepository(db.GetDB())
	ctx := context.Background()
	p := newTestPublication(t, repo, "hr-helper")
	created, err := repo.Upsert(ctx, p)
	require.NoError(t, err)

	other := *p
	other.EnvironmentUUID, other.EnvironmentName, other.DisplayName = uuid.New(), "Production-EU", "Changed"
	_, err = repo.Upsert(ctx, &other)
	assert.ErrorIs(t, err, ErrAgentPublishedFromOtherEnvironment)

	got, err := repo.GetByID(ctx, p.OUID, created.ID)
	require.NoError(t, err)
	assert.Equal(t, p.EnvironmentUUID, got.EnvironmentUUID, "the original environment stays")
	assert.Equal(t, "HR Helper", got.DisplayName, "a rejected publish writes nothing")
}

func TestAgentPublicationUpsertNeverForgetsAnAddedCORSOrigin(t *testing.T) {
	repo := NewAgentPublicationRepository(db.GetDB())
	ctx := context.Background()
	p := newTestPublication(t, repo, "hr-helper")
	p.CORSOriginAdded = true
	_, err := repo.Upsert(ctx, p)
	require.NoError(t, err)

	// On republish the origin is already present, so the service reports it did not add it.
	republish := *p
	republish.CORSOriginAdded = false
	got, err := repo.Upsert(ctx, &republish)
	require.NoError(t, err)
	assert.True(t, got.CORSOriginAdded)
}

func TestAgentPublicationNilTagsAreStoredAsAnEmptyList(t *testing.T) {
	repo := NewAgentPublicationRepository(db.GetDB())
	ctx := context.Background()
	p := newTestPublication(t, repo, "no-tags")
	p.Tags = nil

	created, err := repo.Upsert(ctx, p)
	require.NoError(t, err)
	assert.Equal(t, []string{}, created.Tags)
}

func TestAgentPublicationLookupsAreOrgScoped(t *testing.T) {
	repo := NewAgentPublicationRepository(db.GetDB())
	ctx := context.Background()
	p := newTestPublication(t, repo, "hr-helper")
	created, err := repo.Upsert(ctx, p)
	require.NoError(t, err)

	_, err = repo.GetByID(ctx, "other-org", created.ID)
	assert.ErrorIs(t, err, ErrAgentPublicationNotFound)
	_, err = repo.GetByAgent(ctx, "other-org", p.ProjectName, p.AgentName)
	assert.ErrorIs(t, err, ErrAgentPublicationNotFound)
	_, err = repo.GetByID(ctx, p.OUID, uuid.New())
	assert.ErrorIs(t, err, ErrAgentPublicationNotFound)

	rows, total, err := repo.List(ctx, "other-org", models.AgentPublicationFilter{Limit: 20})
	require.NoError(t, err)
	assert.Zero(t, total)
	assert.Empty(t, rows)
}

func TestAgentPublicationListFiltersPagesAndSortsNewestFirst(t *testing.T) {
	repo := NewAgentPublicationRepository(db.GetDB())
	ctx := context.Background()
	hr := newTestPublication(t, repo, "hr-helper")
	_, err := repo.Upsert(ctx, hr)
	require.NoError(t, err)

	it := newTestPublication(t, repo, "it-helpdesk")
	it.OUID, it.DisplayName, it.Description, it.Tags = hr.OUID, "IT Helpdesk", "Resets passwords (100% self-service)", []string{"IT"}
	_, err = repo.Upsert(ctx, it)
	require.NoError(t, err)

	all, total, err := repo.List(ctx, hr.OUID, models.AgentPublicationFilter{Limit: 20})
	require.NoError(t, err)
	assert.EqualValues(t, 2, total)
	require.Len(t, all, 2)
	assert.Equal(t, "it-helpdesk", all[0].AgentName, "the newest update comes first")

	byTag, total, err := repo.List(ctx, hr.OUID, models.AgentPublicationFilter{Tag: "HR", Limit: 20})
	require.NoError(t, err)
	assert.EqualValues(t, 1, total)
	require.Len(t, byTag, 1)
	assert.Equal(t, "hr-helper", byTag[0].AgentName)

	bySearch, _, err := repo.List(ctx, hr.OUID, models.AgentPublicationFilter{Search: "PASSWORD", Limit: 20})
	require.NoError(t, err)
	require.Len(t, bySearch, 1, "search is case-insensitive and covers the description")
	assert.Equal(t, "it-helpdesk", bySearch[0].AgentName)

	literal, _, err := repo.List(ctx, hr.OUID, models.AgentPublicationFilter{Search: "%", Limit: 20})
	require.NoError(t, err)
	require.Len(t, literal, 1, "% in a search is matched literally, not as a wildcard")

	page, total, err := repo.List(ctx, hr.OUID, models.AgentPublicationFilter{Limit: 1, Offset: 1})
	require.NoError(t, err)
	assert.EqualValues(t, 2, total, "total counts every match, not just the page")
	require.Len(t, page, 1)
	assert.Equal(t, "hr-helper", page[0].AgentName)

	tags, err := repo.ListTags(ctx, hr.OUID)
	require.NoError(t, err)
	assert.Equal(t, []string{"HR", "IT"}, tags)
}

func TestAgentPublicationDeleteReturnsTheRemovedRow(t *testing.T) {
	repo := NewAgentPublicationRepository(db.GetDB())
	ctx := context.Background()
	p := newTestPublication(t, repo, "hr-helper")
	p.CORSOriginAdded = true
	created, err := repo.Upsert(ctx, p)
	require.NoError(t, err)

	_, err = repo.Delete(ctx, p.OUID, p.ProjectName, p.AgentName, uuid.New())
	assert.ErrorIs(t, err, ErrAgentPublicationNotFound, "another environment's delete removes nothing")

	removed, err := repo.Delete(ctx, p.OUID, p.ProjectName, p.AgentName, p.EnvironmentUUID)
	require.NoError(t, err)
	assert.Equal(t, created.ID, removed.ID)
	assert.True(t, removed.CORSOriginAdded, "unpublish learns whether to remove the origin")

	_, err = repo.Delete(ctx, p.OUID, p.ProjectName, p.AgentName, p.EnvironmentUUID)
	assert.ErrorIs(t, err, ErrAgentPublicationNotFound)
}

func TestAgentPublicationDeleteForEnvironmentRemovesItsRows(t *testing.T) {
	repo := NewAgentPublicationRepository(db.GetDB())
	ctx := context.Background()
	p := newTestPublication(t, repo, "hr-helper")
	_, err := repo.Upsert(ctx, p)
	require.NoError(t, err)

	require.NoError(t, repo.DeleteForEnvironment(ctx, p.OUID, p.EnvironmentUUID))
	_, err = repo.GetByAgent(ctx, p.OUID, p.ProjectName, p.AgentName)
	assert.ErrorIs(t, err, ErrAgentPublicationNotFound)
}
