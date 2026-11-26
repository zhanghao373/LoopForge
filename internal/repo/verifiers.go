package repo

import (
	"errors"
	"fmt"
	"time"

	"agentflow/internal/domain"

	"gorm.io/gorm"
)

type verifierRow struct {
	ID        string     `gorm:"column:id;primaryKey;size:64"`
	ProjectID string     `gorm:"column:project_id;size:64;not null;index"`
	Payload   []byte     `gorm:"column:payload;type:json"`
	Updated   *time.Time `gorm:"column:updated_at"`
}

func (verifierRow) TableName() string { return "verifiers" }

var ErrVerifierConflict = errors.New("Verifier 已被修改或标识已存在，请刷新后重试")

func (r *CatalogRepo) Verifiers(projectID string) []domain.VerifierDefinition {
	db := r.DB.Order("updated_at desc")
	if projectID != "" {
		db = db.Where("project_id = ?", projectID)
	}
	return payloadList[domain.VerifierDefinition](db, "verifiers", "")
}

func (r *CatalogRepo) Verifier(id string) (domain.VerifierDefinition, bool) {
	return payloadOne[domain.VerifierDefinition](r.DB, "verifiers", id)
}

func (r *CatalogRepo) PutVerifier(v domain.VerifierDefinition) (domain.VerifierDefinition, error) {
	if err := v.Validate(); err != nil {
		return domain.VerifierDefinition{}, err
	}
	err := r.DB.Transaction(func(tx *gorm.DB) error {
		var row verifierRow
		err := r.locking(tx).Where("id = ?", v.ID).Take(&row).Error
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			if v.Version != 0 {
				return ErrVerifierConflict
			}
		case err != nil:
			return err
		default:
			current, found := payloadOne[domain.VerifierDefinition](tx, "verifiers", v.ID)
			if !found || current.Version != v.Version || current.ProjectID != v.ProjectID {
				return ErrVerifierConflict
			}
		}
		v.Version++
		v.UpdatedAt = time.Now().UTC()
		record := verifierRow{ID: v.ID, ProjectID: v.ProjectID, Payload: mustJSON(v), Updated: &v.UpdatedAt}
		if v.Version == 1 {
			return tx.Create(&record).Error
		}
		return tx.Model(&row).Updates(&record).Error
	})
	if err != nil {
		return domain.VerifierDefinition{}, err
	}
	return v, nil
}

// WorkflowVerifiers resolves each distinct reference once, scoped to its
// project. An explicit SOP refresh can carry forward versions used by the run.
func (r *CatalogRepo) WorkflowVerifiers(wf domain.WorkflowDefinition, pinned map[string]domain.VerifierDefinition) (map[string]domain.VerifierDefinition, error) {
	var snapshots map[string]domain.VerifierDefinition
	for _, node := range wf.Nodes {
		if err := node.ValidateVerification(); err != nil {
			return nil, err
		}
		if node.Verification == nil {
			continue
		}
		for _, ref := range node.Verification.Verifiers {
			if _, ok := snapshots[ref.VerifierID]; ok {
				continue
			}
			if wf.ProjectID == "" || wf.Generic || wf.Shared {
				return nil, fmt.Errorf("节点「%s」的 Verifier 只能在所属项目的 SOP 中引用", node.Name)
			}
			v, ok := pinned[ref.VerifierID]
			if !ok {
				v, ok = r.Verifier(ref.VerifierID)
			}
			if !ok || v.ProjectID != wf.ProjectID {
				return nil, fmt.Errorf("节点「%s」无法引用本项目的 Verifier %q", node.Name, ref.VerifierID)
			}
			if snapshots == nil {
				snapshots = make(map[string]domain.VerifierDefinition)
			}
			snapshots[v.ID] = v
		}
	}
	return snapshots, nil
}
