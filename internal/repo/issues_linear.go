package repo

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"agentflow/internal/domain"

	"gorm.io/gorm"
)

type LinearImportResult struct {
	Created   int `json:"created"`
	Updated   int `json:"updated"`
	Unchanged int `json:"unchanged"`
}

// ImportLinearIssues refreshes only the fields owned by Linear. The project
// row serializes imports from different processes into the same board.
func (r *IssuesRepo) ImportLinearIssues(projectID string, issues []domain.Issue) (LinearImportResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	var result LinearImportResult
	err := r.DB.Transaction(func(tx *gorm.DB) error {
		var project projectRow
		if err := r.locking(tx).Select("id").Where("id = ?", projectID).Take(&project).Error; err != nil {
			return err
		}
		d := &deps{DB: tx}
		scoped := IssuesRepo{deps: d, projects: &ProjectsRepo{deps: d}}
		for _, incoming := range issues {
			ref := incoming.ExternalRef
			if ref == nil || ref.Source != "linear" || ref.ExternalID == "" || ref.ExternalUpdatedAt == nil {
				return errors.New("missing Linear issue identity")
			}
			hash := sha256.Sum256([]byte(projectID + "\x00" + ref.ExternalID))
			incoming.ID = "linear-" + hex.EncodeToString(hash[:24])
			incoming.ProjectID = projectID
			incoming.Source = "linear"

			var row issueRow
			err := scoped.locking(tx).Where("id = ?", incoming.ID).Take(&row).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				if err := scoped.saveIssueLocked(incoming, true, false); err != nil {
					return err
				}
				result.Created++
				continue
			}
			if err != nil {
				return err
			}
			var current domain.Issue
			if err := jsonUnmarshal(row.Payload, &current); err != nil {
				return err
			}
			if current.Source != "linear" || current.ProjectID != projectID || current.ExternalRef == nil || current.ExternalRef.ExternalID != ref.ExternalID {
				return errors.New("Linear issue identity conflicts with an existing issue")
			}
			if updated := current.ExternalRef.ExternalUpdatedAt; updated != nil && !ref.ExternalUpdatedAt.After(*updated) {
				result.Unchanged++
				continue
			}
			current.Title = incoming.Title
			current.Description = incoming.Description
			current.Priority = incoming.Priority
			current.ExternalRef = ref
			current.UpdatedAt = time.Now().UTC()
			if err := tx.Model(&issueRow{}).Where("id = ?", row.ID).Updates(map[string]any{
				"title": current.Title, "payload": mustJSON(current), "updated_at": current.UpdatedAt,
			}).Error; err != nil {
				return err
			}
			result.Updated++
		}
		return nil
	})
	if err != nil {
		return LinearImportResult{}, err
	}
	return result, nil
}
