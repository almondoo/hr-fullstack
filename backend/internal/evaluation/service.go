package evaluation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/approval"
	"github.com/your-org/hr-saas/internal/platform/audit"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// Sentinel errors.
var (
	ErrNotFound          = errors.New("evaluation: not found")
	ErrInvalidTransition = errors.New("evaluation: invalid review status transition")
	ErrForbidden         = errors.New("evaluation: permission denied")
	ErrConfirmed         = errors.New("evaluation: review is confirmed (read-only)")
	ErrIncomplete        = errors.New("evaluation: stage has unanswered items")
)

// minAnonymousResponses is the default minimum number of submitted 360 responses
// required before anonymous aggregation results are shown.  Below this threshold
// individual raters could be re-identified, so results are suppressed.
//
// 制度 note: this floor is a privacy default, not a legal value; it is exposed
// as a parameter to Aggregate360 so tenants can raise it per their 360 policy.
const minAnonymousResponses = 3

// allowedReviewTransitions defines the legal review-status FSM moves.
// not_started → self_submitted → primary_submitted → secondary_submitted
//
//	→ calibrated → confirmed
//
// confirmed is terminal (read-only).
var allowedReviewTransitions = map[string]map[string]bool{
	StatusNotStarted: {
		StatusSelfSubmitted: true,
	},
	StatusSelfSubmitted: {
		StatusPrimarySubmitted: true,
		// primary reviewer can return to the subject (back to not_started for redo)
		StatusNotStarted: true,
	},
	StatusPrimarySubmitted: {
		StatusSecondarySubmitted: true,
		// secondary reviewer can return to primary
		StatusSelfSubmitted: true,
	},
	StatusSecondarySubmitted: {
		StatusCalibrated: true,
		StatusConfirmed:  true,
		// allow returning to primary stage
		StatusPrimarySubmitted: true,
	},
	StatusCalibrated: {
		StatusConfirmed: true,
	},
}

// stageForSubmission maps a target submission status to the stage whose entries
// must be complete before the transition is allowed.
var stageForSubmission = map[string]string{
	StatusSelfSubmitted:      StageSelf,
	StatusPrimarySubmitted:   StagePrimary,
	StatusSecondarySubmitted: StageSecondary,
}

// isReviewTransitionAllowed reports whether moving from current → next is legal.
func isReviewTransitionAllowed(current, next string) bool {
	if nexts, ok := allowedReviewTransitions[current]; ok {
		return nexts[next]
	}
	return false
}

// templateItem is the shape of each element in a template's items_json.
// Weight drives the weighted-average aggregation; CompetencyKey / GradeKey link
// to competency / grade definitions (config-driven, not hardcoded).
type templateItem struct {
	ItemKey       string  `json:"item_key"`
	Weight        float64 `json:"weight"`
	CompetencyKey string  `json:"competency_key,omitempty"`
	GradeKey      string  `json:"grade_key,omitempty"`
}

// Service provides business logic for the evaluation domain.
type Service struct {
	tdb         *tenantdb.TenantDB
	approvalSvc *approval.Service
}

// NewService constructs a Service.  The approval engine is self-constructed here
// (not passed in) so that the package keeps the unified RegisterRoutes signature.
func NewService(tdb *tenantdb.TenantDB) *Service {
	return &Service{
		tdb:         tdb,
		approvalSvc: approval.NewService(tdb),
	}
}

// ---------------------------------------------------------------------------
// Templates
// ---------------------------------------------------------------------------

// CreateTemplateInput holds fields for creating a review template.
type CreateTemplateInput struct {
	TenantID        uuid.UUID
	ActorID         uuid.UUID
	Name            string
	StagesJSON      []byte
	ItemsJSON       []byte
	RatingScaleJSON []byte
	IP              *string
}

// CreateTemplate creates a new review template.
func (s *Service) CreateTemplate(ctx context.Context, in CreateTemplateInput) (*Template, error) {
	tmpl := Template{
		ID:              uuid.New(),
		TenantID:        in.TenantID,
		Name:            in.Name,
		StagesJSON:      defaultJSON(in.StagesJSON, "[]"),
		ItemsJSON:       defaultJSON(in.ItemsJSON, "[]"),
		RatingScaleJSON: defaultJSON(in.RatingScaleJSON, "{}"),
		Active:          true,
	}
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		if err := tx.Exec(
			`INSERT INTO review_templates
			   (id, tenant_id, name, stages_json, items_json, rating_scale_json, active)
			 VALUES (?, ?, ?, ?::jsonb, ?::jsonb, ?::jsonb, ?)`,
			tmpl.ID, tmpl.TenantID, tmpl.Name,
			tmpl.StagesJSON, tmpl.ItemsJSON, tmpl.RatingScaleJSON, tmpl.Active,
		).Error; err != nil {
			return fmt.Errorf("evaluation: create template insert: %w", err)
		}
		idStr := tmpl.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "review_template.created",
			ResourceType: "review_template",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &tmpl, nil
}

// ListTemplates returns active templates for a tenant.
func (s *Service) ListTemplates(ctx context.Context, tenantID uuid.UUID) ([]Template, error) {
	var tmpls []Template
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, name, stages_json, items_json, rating_scale_json,
			        active, created_at, updated_at
			 FROM review_templates
			 WHERE tenant_id = ? AND active = true
			 ORDER BY name`,
			tenantID,
		).Scan(&tmpls).Error
	})
	if err != nil {
		return nil, err
	}
	return tmpls, nil
}

// GetTemplate fetches a single template by ID within the tenant.
func (s *Service) GetTemplate(ctx context.Context, tenantID, id uuid.UUID) (*Template, error) {
	var tmpl Template
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, name, stages_json, items_json, rating_scale_json,
			        active, created_at, updated_at
			 FROM review_templates WHERE id = ? AND tenant_id = ? LIMIT 1`,
			id, tenantID,
		).Scan(&tmpl).Error
	})
	if err != nil {
		return nil, err
	}
	if tmpl.ID == uuid.Nil {
		return nil, ErrNotFound
	}
	return &tmpl, nil
}

// ---------------------------------------------------------------------------
// Reviews
// ---------------------------------------------------------------------------

// CreateReviewInput holds fields for creating a review header.
type CreateReviewInput struct {
	TenantID            uuid.UUID
	ActorID             uuid.UUID
	CycleID             uuid.UUID
	TemplateID          uuid.UUID
	EmployeeID          uuid.UUID
	PrimaryReviewerID   *uuid.UUID
	SecondaryReviewerID *uuid.UUID
	IP                  *string
}

// CreateReview creates a review header for (cycle, employee).
// The subject employee, template, and reviewers must belong to the same tenant
// (enforced by composite FKs and explicit existence checks).
func (s *Service) CreateReview(ctx context.Context, in CreateReviewInput) (*Review, error) {
	review := Review{
		ID:                  uuid.New(),
		TenantID:            in.TenantID,
		CycleID:             in.CycleID,
		TemplateID:          in.TemplateID,
		EmployeeID:          in.EmployeeID,
		PrimaryReviewerID:   in.PrimaryReviewerID,
		SecondaryReviewerID: in.SecondaryReviewerID,
		Status:              StatusNotStarted,
	}
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		if err := requireEmployee(tx, in.TenantID, in.EmployeeID); err != nil {
			return err
		}
		if err := requireTemplate(tx, in.TenantID, in.TemplateID); err != nil {
			return err
		}
		if in.PrimaryReviewerID != nil {
			if err := requireEmployee(tx, in.TenantID, *in.PrimaryReviewerID); err != nil {
				return err
			}
		}
		if in.SecondaryReviewerID != nil {
			if err := requireEmployee(tx, in.TenantID, *in.SecondaryReviewerID); err != nil {
				return err
			}
		}

		if err := tx.Exec(
			`INSERT INTO reviews
			   (id, tenant_id, cycle_id, template_id, employee_id,
			    primary_reviewer_id, secondary_reviewer_id, status)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			review.ID, review.TenantID, review.CycleID, review.TemplateID,
			review.EmployeeID, review.PrimaryReviewerID, review.SecondaryReviewerID,
			review.Status,
		).Error; err != nil {
			return fmt.Errorf("evaluation: create review insert: %w", err)
		}
		idStr := review.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "review.created",
			ResourceType: "review",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &review, nil
}

// GetReview fetches a single review by ID within the tenant.
func (s *Service) GetReview(ctx context.Context, tenantID, id uuid.UUID) (*Review, error) {
	var review Review
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(reviewSelectByID, id, tenantID).Scan(&review).Error
	})
	if err != nil {
		return nil, err
	}
	if review.ID == uuid.Nil {
		return nil, ErrNotFound
	}
	return &review, nil
}

// ListReviewsByCycle lists all reviews for a cycle within the tenant.
func (s *Service) ListReviewsByCycle(ctx context.Context, tenantID, cycleID uuid.UUID) ([]Review, error) {
	var reviews []Review
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, cycle_id, template_id, employee_id,
			        primary_reviewer_id, secondary_reviewer_id, status,
			        final_rating, adjusted_rating, confirmed_at, created_at, updated_at
			 FROM reviews
			 WHERE tenant_id = ? AND cycle_id = ?
			 ORDER BY created_at`,
			tenantID, cycleID,
		).Scan(&reviews).Error
	})
	if err != nil {
		return nil, err
	}
	return reviews, nil
}

const reviewSelectByID = `SELECT id, tenant_id, cycle_id, template_id, employee_id,
       primary_reviewer_id, secondary_reviewer_id, status,
       final_rating, adjusted_rating, confirmed_at, created_at, updated_at
 FROM reviews WHERE id = ? AND tenant_id = ? LIMIT 1`

// ---------------------------------------------------------------------------
// Entries (per-item answers)
// ---------------------------------------------------------------------------

// UpsertEntryInput holds fields for recording a single per-item answer.
type UpsertEntryInput struct {
	TenantID       uuid.UUID
	ActorID        uuid.UUID
	ReviewID       uuid.UUID
	Stage          string
	ReviewerUserID *uuid.UUID
	ItemKey        string
	Score          *float64
	Comment        *string
	IP             *string
}

// UpsertEntry records (or updates) one evaluation answer.
// Entries may only be written while the review is NOT confirmed and the stage
// has not yet been submitted past it (i.e. self entries lock after
// self_submitted).  The comment body is stored but never appears in audit logs.
func (s *Service) UpsertEntry(ctx context.Context, in UpsertEntryInput) (*Entry, error) {
	if _, ok := map[string]bool{StageSelf: true, StagePrimary: true, StageSecondary: true, Stage360: true}[in.Stage]; !ok {
		return nil, fmt.Errorf("%w: unknown stage %q", ErrInvalidTransition, in.Stage)
	}

	var entry Entry
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		review, err := loadReviewForUpdate(tx, in.TenantID, in.ReviewID)
		if err != nil {
			return err
		}
		if review.Status == StatusConfirmed {
			return ErrConfirmed
		}
		// Stage edit-lock: once a stage has been submitted, its entries lock.
		if isStageLocked(review.Status, in.Stage) {
			return fmt.Errorf("%w: stage %q is locked at review status %q", ErrInvalidTransition, in.Stage, review.Status)
		}

		entry = Entry{
			ID:             uuid.New(),
			TenantID:       in.TenantID,
			ReviewID:       in.ReviewID,
			Stage:          in.Stage,
			ReviewerUserID: in.ReviewerUserID,
			ItemKey:        in.ItemKey,
			Score:          in.Score,
			Comment:        in.Comment,
		}
		if err := tx.Exec(
			`INSERT INTO review_entries
			   (id, tenant_id, review_id, stage, reviewer_user_id, item_key, score, comment)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT (review_id, stage, reviewer_user_id, item_key, tenant_id) DO UPDATE
			   SET score      = EXCLUDED.score,
			       comment    = EXCLUDED.comment,
			       updated_at = now()`,
			entry.ID, entry.TenantID, entry.ReviewID, entry.Stage,
			entry.ReviewerUserID, entry.ItemKey, entry.Score, entry.Comment,
		).Error; err != nil {
			return fmt.Errorf("evaluation: upsert entry: %w", err)
		}

		// Re-read the persisted row (handles the upsert case).
		if err := tx.Raw(
			`SELECT id, tenant_id, review_id, stage, reviewer_user_id, item_key,
			        score, comment, submitted_at, created_at, updated_at
			 FROM review_entries
			 WHERE review_id = ? AND tenant_id = ? AND stage = ? AND item_key = ?
			   AND reviewer_user_id IS NOT DISTINCT FROM ?
			 LIMIT 1`,
			in.ReviewID, in.TenantID, in.Stage, in.ItemKey, in.ReviewerUserID,
		).Scan(&entry).Error; err != nil {
			return fmt.Errorf("evaluation: upsert entry re-read: %w", err)
		}

		// Audit: opaque review ID only — never the comment body or score detail.
		idStr := in.ReviewID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "review_entry.upserted",
			ResourceType: "review",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &entry, nil
}

// ListEntries returns entries for a review, optionally filtered by stage.
//
// 360 anonymity guarantee: this generic endpoint NEVER returns 360-stage rows.
// A 360 entry's reviewer_user_id (and free-text comment) would otherwise let any
// review:read holder de-anonymize an anonymous rater, bypassing the suppression
// that Aggregate360 implements.  All 360 reads MUST go through Aggregate360,
// which is anonymity-safe.  Therefore:
//   - an explicit stage=="360" request is rejected with ErrForbidden; and
//   - unfiltered / other-stage queries exclude stage='360' rows entirely at the
//     SQL level (defence-in-depth — the column physically cannot leak here).
func (s *Service) ListEntries(ctx context.Context, tenantID, reviewID uuid.UUID, stage string) ([]Entry, error) {
	// 360-stage rows are only ever readable via the anonymity-safe Aggregate360.
	if stage == Stage360 {
		return nil, fmt.Errorf("%w: 360-stage entries must be read via Aggregate360", ErrForbidden)
	}

	var entries []Entry
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		// Exclude 360-stage rows unconditionally so reviewer_user_id / comment of
		// (anonymous or named) 360 raters can never surface through this path.
		q := `SELECT id, tenant_id, review_id, stage, reviewer_user_id, item_key,
		             score, comment, submitted_at, created_at, updated_at
		      FROM review_entries
		      WHERE tenant_id = ? AND review_id = ? AND stage <> ?`
		args := []any{tenantID, reviewID, Stage360}
		if stage != "" {
			q += ` AND stage = ?`
			args = append(args, stage)
		}
		q += ` ORDER BY stage, item_key`
		return tx.Raw(q, args...).Scan(&entries).Error
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

// ---------------------------------------------------------------------------
// Stage submission (FSM + approval engine integration)
// ---------------------------------------------------------------------------

// SubmitStageInput holds fields for submitting a review stage.
type SubmitStageInput struct {
	TenantID   uuid.UUID
	ActorID    uuid.UUID
	ReviewID   uuid.UUID
	NextStatus string
	// DepartmentID is used by the approval engine for route resolution (optional).
	DepartmentID *uuid.UUID
	IP           *string
}

// SubmitStage advances the review FSM to NextStatus.  Before advancing it
// verifies that every template item has an answer for the corresponding stage
// (for self/primary/secondary submissions).  The state change is recorded
// atomically together with an approval-engine submission (when a route is
// configured) so the approval request and the review update commit or roll
// back together.
//
// Returning to an earlier stage (差戻し) is also a transition handled here; the
// approval engine's return semantics are exercised via the same SubmitTx path.
func (s *Service) SubmitStage(ctx context.Context, in SubmitStageInput) (*Review, error) {
	var updated Review
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		review, err := loadReviewForUpdate(tx, in.TenantID, in.ReviewID)
		if err != nil {
			return err
		}
		if review.Status == StatusConfirmed {
			return ErrConfirmed
		}
		if !isReviewTransitionAllowed(review.Status, in.NextStatus) {
			return fmt.Errorf("%w: %s → %s", ErrInvalidTransition, review.Status, in.NextStatus)
		}

		// For forward stage submissions, verify the stage's items are complete.
		if stage, ok := stageForSubmission[in.NextStatus]; ok {
			complete, err := stageItemsComplete(tx, in.TenantID, review, stage)
			if err != nil {
				return err
			}
			if !complete {
				return fmt.Errorf("%w: stage %q", ErrIncomplete, stage)
			}
		}

		// Mark the stage entries as submitted (lock) when moving forward.
		if stage, ok := stageForSubmission[in.NextStatus]; ok {
			if err := tx.Exec(
				`UPDATE review_entries
				 SET submitted_at = now(), updated_at = now()
				 WHERE review_id = ? AND tenant_id = ? AND stage = ? AND submitted_at IS NULL`,
				in.ReviewID, in.TenantID, stage,
			).Error; err != nil {
				return fmt.Errorf("evaluation: submit stage lock entries: %w", err)
			}
		}

		res := tx.Exec(
			`UPDATE reviews SET status = ?, updated_at = now()
			 WHERE id = ? AND tenant_id = ?`,
			in.NextStatus, in.ReviewID, in.TenantID,
		)
		if res.Error != nil {
			return fmt.Errorf("evaluation: submit stage update: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}

		// Integrate the approval engine atomically.  A missing route is not an
		// error — manually-managed tenants proceed without an approval link.
		_, submitErr := s.approvalSvc.SubmitTx(tx, approval.SubmitInput{
			TenantID:     in.TenantID,
			ActorID:      in.ActorID,
			RequestType:  "review_" + in.NextStatus,
			SubjectRef:   in.ReviewID.String(),
			DepartmentID: in.DepartmentID,
			// PayloadJSON: reference IDs only — never評価comment本文/score.
			PayloadJSON: []byte(`{"review_id":"` + in.ReviewID.String() + `","stage":"` + in.NextStatus + `"}`),
			IP:          in.IP,
		})
		if submitErr != nil &&
			!errors.Is(submitErr, approval.ErrRouteNotFound) &&
			!errors.Is(submitErr, approval.ErrRouteEmpty) {
			return fmt.Errorf("evaluation: submit stage approval: %w", submitErr)
		}

		if err := tx.Raw(reviewSelectByID, in.ReviewID, in.TenantID).Scan(&updated).Error; err != nil {
			return fmt.Errorf("evaluation: submit stage re-read: %w", err)
		}

		idStr := in.ReviewID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "review.stage_submitted",
			ResourceType: "review",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &updated, nil
}

// ---------------------------------------------------------------------------
// Confirmation (compute final_rating via config-driven weighted average)
// ---------------------------------------------------------------------------

// ConfirmReviewInput holds fields for confirming a review.
type ConfirmReviewInput struct {
	TenantID uuid.UUID
	ActorID  uuid.UUID
	ReviewID uuid.UUID
	IP       *string
}

// ConfirmReview computes the final_rating as a config-driven weighted average of
// the secondary-stage item scores (weights and rating-scale mapping come from
// the template's items_json / rating_scale_json — never hardcoded) and marks the
// review confirmed (read-only).  Only secondary_submitted or calibrated reviews
// may be confirmed.
func (s *Service) ConfirmReview(ctx context.Context, in ConfirmReviewInput) (*Review, error) {
	var updated Review
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		review, err := loadReviewForUpdate(tx, in.TenantID, in.ReviewID)
		if err != nil {
			return err
		}
		if review.Status == StatusConfirmed {
			return ErrConfirmed
		}
		if !isReviewTransitionAllowed(review.Status, StatusConfirmed) {
			return fmt.Errorf("%w: %s → %s", ErrInvalidTransition, review.Status, StatusConfirmed)
		}

		// Compute the weighted average from the template config + secondary entries.
		final, err := s.computeFinalRating(tx, in.TenantID, review)
		if err != nil {
			return err
		}

		res := tx.Exec(
			`UPDATE reviews
			 SET status = ?, final_rating = ?, confirmed_at = now(), updated_at = now()
			 WHERE id = ? AND tenant_id = ?`,
			StatusConfirmed, final, in.ReviewID, in.TenantID,
		)
		if res.Error != nil {
			return fmt.Errorf("evaluation: confirm review update: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}

		if err := tx.Raw(reviewSelectByID, in.ReviewID, in.TenantID).Scan(&updated).Error; err != nil {
			return fmt.Errorf("evaluation: confirm review re-read: %w", err)
		}

		idStr := in.ReviewID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "review.confirmed",
			ResourceType: "review",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &updated, nil
}

// computeFinalRating resolves item weights and the rating-scale numeric mapping
// from the review's template, then computes the weighted average of the
// secondary-stage scores.  All values are config-driven (items_json /
// rating_scale_json); nothing is hardcoded.
func (s *Service) computeFinalRating(tx *gorm.DB, tenantID uuid.UUID, review *Review) (*float64, error) {
	var tmpl Template
	if err := tx.Raw(
		`SELECT id, tenant_id, items_json, rating_scale_json
		 FROM review_templates WHERE id = ? AND tenant_id = ? LIMIT 1`,
		review.TemplateID, tenantID,
	).Scan(&tmpl).Error; err != nil {
		return nil, fmt.Errorf("evaluation: compute final fetch template: %w", err)
	}
	if tmpl.ID == uuid.Nil {
		return nil, ErrNotFound
	}

	var items []templateItem
	if len(tmpl.ItemsJSON) > 0 {
		if err := json.Unmarshal(tmpl.ItemsJSON, &items); err != nil {
			return nil, fmt.Errorf("evaluation: compute final parse items_json: %w", err)
		}
	}
	weights := make(map[string]float64, len(items))
	for _, it := range items {
		weights[it.ItemKey] = it.Weight
	}

	// Load secondary-stage scores for this review.
	var rows []struct {
		ItemKey string   `gorm:"column:item_key"`
		Score   *float64 `gorm:"column:score"`
	}
	if err := tx.Raw(
		`SELECT item_key, score FROM review_entries
		 WHERE review_id = ? AND tenant_id = ? AND stage = ?`,
		review.ID, tenantID, StageSecondary,
	).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("evaluation: compute final load entries: %w", err)
	}

	var weightedSum, totalWeight float64
	for _, r := range rows {
		if r.Score == nil {
			continue
		}
		w, ok := weights[r.ItemKey]
		if !ok || w == 0 {
			// Items without a configured weight default to weight 1 so an
			// un-weighted template still produces a simple average.
			w = 1
		}
		weightedSum += (*r.Score) * w
		totalWeight += w
	}
	if totalWeight == 0 {
		// No scored secondary entries — leave final_rating NULL.
		return nil, nil
	}
	avg := weightedSum / totalWeight
	return &avg, nil
}

// ---------------------------------------------------------------------------
// 360-degree review
// ---------------------------------------------------------------------------

// Create360RequestInput holds fields for inviting a 360 rater.
type Create360RequestInput struct {
	TenantID        uuid.UUID
	ActorID         uuid.UUID
	ReviewID        uuid.UUID
	RaterEmployeeID uuid.UUID
	Relationship    string
	Anonymous       bool
	IP              *string
}

// Create360Request invites a rater (同僚/部下/他部署) to a 360 review.  The
// rater must be a same-tenant employee (composite FK + explicit check).
func (s *Service) Create360Request(ctx context.Context, in Create360RequestInput) (*Request360, error) {
	rel := in.Relationship
	if rel == "" {
		rel = "peer"
	}
	if _, ok := map[string]bool{"peer": true, "subordinate": true, "other": true}[rel]; !ok {
		return nil, fmt.Errorf("%w: unknown relationship %q", ErrInvalidTransition, rel)
	}

	req := Request360{
		ID:              uuid.New(),
		TenantID:        in.TenantID,
		ReviewID:        in.ReviewID,
		RaterEmployeeID: in.RaterEmployeeID,
		Relationship:    rel,
		Anonymous:       in.Anonymous,
		Status:          Req360Pending,
	}
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		if _, err := loadReviewForUpdate(tx, in.TenantID, in.ReviewID); err != nil {
			return err
		}
		if err := requireEmployee(tx, in.TenantID, in.RaterEmployeeID); err != nil {
			return err
		}
		if err := tx.Exec(
			`INSERT INTO review_360_requests
			   (id, tenant_id, review_id, rater_employee_id, relationship, anonymous, status)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			req.ID, req.TenantID, req.ReviewID, req.RaterEmployeeID,
			req.Relationship, req.Anonymous, req.Status,
		).Error; err != nil {
			return fmt.Errorf("evaluation: create 360 request insert: %w", err)
		}
		idStr := req.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "review_360_request.created",
			ResourceType: "review_360_request",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &req, nil
}

// Submit360ResponseInput holds fields for a rater submitting their 360 answers.
type Submit360ResponseInput struct {
	TenantID  uuid.UUID
	ActorID   uuid.UUID
	RequestID uuid.UUID
	// Entries are the per-item answers; reviewer attribution is taken from the
	// request's rater so anonymity can be enforced at aggregation time.
	Entries []Item360
	IP      *string
}

// Item360 is one answer in a 360 response.
type Item360 struct {
	ItemKey string
	Score   *float64
	Comment *string
}

// Submit360Response records a rater's 360 answers and marks the request
// submitted.  The reviewer_user_id is stored for integrity but is suppressed in
// aggregation for anonymous requests.
func (s *Service) Submit360Response(ctx context.Context, in Submit360ResponseInput) error {
	return s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		var req Request360
		if err := tx.Raw(
			`SELECT id, tenant_id, review_id, rater_employee_id, relationship,
			        anonymous, status, responded_at, created_at, updated_at
			 FROM review_360_requests WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.RequestID, in.TenantID,
		).Scan(&req).Error; err != nil {
			return fmt.Errorf("evaluation: submit 360 load request: %w", err)
		}
		if req.ID == uuid.Nil {
			return ErrNotFound
		}
		if req.Status == Req360Submitted {
			return fmt.Errorf("%w: 360 request already submitted", ErrInvalidTransition)
		}

		// The rater's user attribution: we record the actor as reviewer_user_id.
		reviewerUserID := in.ActorID
		for _, item := range in.Entries {
			entryID := uuid.New()
			if err := tx.Exec(
				`INSERT INTO review_entries
				   (id, tenant_id, review_id, stage, reviewer_user_id, item_key, score, comment, submitted_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, now())
				 ON CONFLICT (review_id, stage, reviewer_user_id, item_key, tenant_id) DO UPDATE
				   SET score        = EXCLUDED.score,
				       comment      = EXCLUDED.comment,
				       submitted_at = now(),
				       updated_at   = now()`,
				entryID, in.TenantID, req.ReviewID, Stage360, reviewerUserID,
				item.ItemKey, item.Score, item.Comment,
			).Error; err != nil {
				return fmt.Errorf("evaluation: submit 360 insert entry: %w", err)
			}
		}

		res := tx.Exec(
			`UPDATE review_360_requests
			 SET status = ?, responded_at = now(), updated_at = now()
			 WHERE id = ? AND tenant_id = ?`,
			Req360Submitted, in.RequestID, in.TenantID,
		)
		if res.Error != nil {
			return fmt.Errorf("evaluation: submit 360 update request: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}

		idStr := in.RequestID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "review_360_request.responded",
			ResourceType: "review_360_request",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
}

// Aggregate360Result is the anonymity-safe aggregation of 360 responses.
//
// When the number of submitted responses is below minResponses (anonymous
// floor), Suppressed is true and the per-item averages are withheld so that an
// individual anonymous rater cannot be re-identified.  RaterIDs are NEVER
// included for anonymous requests.
type Aggregate360Result struct {
	ReviewID      uuid.UUID
	ResponseCount int
	Suppressed    bool
	ItemAverages  map[string]float64
	// RaterEmployeeIDs lists raters ONLY for non-anonymous requests.  It is
	// always empty when any contributing request is anonymous.
	RaterEmployeeIDs []uuid.UUID
}

// Aggregate360 computes per-item average scores across submitted 360 responses.
// minResponses overrides the default anonymity floor (use 0 for the default).
//
// Anonymity guarantee: rater_employee_id / reviewer_user_id are never returned
// when any contributing request is anonymous, and when the submitted-response
// count is below the floor the averages themselves are suppressed.
func (s *Service) Aggregate360(ctx context.Context, tenantID, reviewID uuid.UUID, minResponses int) (*Aggregate360Result, error) {
	floor := minResponses
	if floor <= 0 {
		floor = minAnonymousResponses
	}

	result := &Aggregate360Result{
		ReviewID:     reviewID,
		ItemAverages: map[string]float64{},
	}
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		if _, err := loadReviewForUpdate(tx, tenantID, reviewID); err != nil {
			return err
		}

		// Count submitted responses and detect whether any are anonymous.
		var reqRows []struct {
			RaterEmployeeID uuid.UUID `gorm:"column:rater_employee_id"`
			Anonymous       bool      `gorm:"column:anonymous"`
		}
		if err := tx.Raw(
			`SELECT rater_employee_id, anonymous FROM review_360_requests
			 WHERE review_id = ? AND tenant_id = ? AND status = ?`,
			reviewID, tenantID, Req360Submitted,
		).Scan(&reqRows).Error; err != nil {
			return fmt.Errorf("evaluation: aggregate 360 load requests: %w", err)
		}
		result.ResponseCount = len(reqRows)

		anyAnonymous := false
		var raterIDs []uuid.UUID
		for _, r := range reqRows {
			if r.Anonymous {
				anyAnonymous = true
			}
			raterIDs = append(raterIDs, r.RaterEmployeeID)
		}

		// Suppress everything below the floor (privacy).
		if result.ResponseCount < floor {
			result.Suppressed = true
			return nil
		}

		// Compute per-item averages from 360-stage entries.
		var entryRows []struct {
			ItemKey string   `gorm:"column:item_key"`
			Score   *float64 `gorm:"column:score"`
		}
		if err := tx.Raw(
			`SELECT item_key, score FROM review_entries
			 WHERE review_id = ? AND tenant_id = ? AND stage = ?`,
			reviewID, tenantID, Stage360,
		).Scan(&entryRows).Error; err != nil {
			return fmt.Errorf("evaluation: aggregate 360 load entries: %w", err)
		}

		sums := map[string]float64{}
		counts := map[string]int{}
		for _, e := range entryRows {
			if e.Score == nil {
				continue
			}
			sums[e.ItemKey] += *e.Score
			counts[e.ItemKey]++
		}
		for k, sum := range sums {
			result.ItemAverages[k] = sum / float64(counts[k])
		}

		// Rater IDs are only safe to expose when NO contributing request is
		// anonymous.  Otherwise the slice stays empty (秘匿).
		if !anyAnonymous {
			sort.Slice(raterIDs, func(i, j int) bool {
				return raterIDs[i].String() < raterIDs[j].String()
			})
			result.RaterEmployeeIDs = raterIDs
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// ---------------------------------------------------------------------------
// Calibration
// ---------------------------------------------------------------------------

// CreateCalibrationInput holds fields for opening a calibration session.
type CreateCalibrationInput struct {
	TenantID          uuid.UUID
	ActorID           uuid.UUID
	CycleID           uuid.UUID
	Name              string
	FacilitatorUserID *uuid.UUID
	IP                *string
}

// CreateCalibrationSession opens a calibration (評価会議) session for a cycle.
func (s *Service) CreateCalibrationSession(ctx context.Context, in CreateCalibrationInput) (*CalibrationSession, error) {
	sess := CalibrationSession{
		ID:                uuid.New(),
		TenantID:          in.TenantID,
		CycleID:           in.CycleID,
		Name:              in.Name,
		FacilitatorUserID: in.FacilitatorUserID,
		Status:            CalibrationOpen,
		DecisionsJSON:     []byte(`[]`),
	}
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		if err := tx.Exec(
			`INSERT INTO calibration_sessions
			   (id, tenant_id, cycle_id, name, facilitator_user_id, status, decisions_json)
			 VALUES (?, ?, ?, ?, ?, ?, ?::jsonb)`,
			sess.ID, sess.TenantID, sess.CycleID, sess.Name,
			sess.FacilitatorUserID, sess.Status, sess.DecisionsJSON,
		).Error; err != nil {
			return fmt.Errorf("evaluation: create calibration insert: %w", err)
		}
		idStr := sess.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "calibration_session.created",
			ResourceType: "calibration_session",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &sess, nil
}

// CalibrationDecision is one row in a calibration session's decisions_json.
type CalibrationDecision struct {
	ReviewID string  `json:"review_id"`
	Before   float64 `json:"before"`
	After    float64 `json:"after"`
	Reason   string  `json:"reason"`
}

// ApplyCalibrationInput holds fields for applying a calibration adjustment.
type ApplyCalibrationInput struct {
	TenantID  uuid.UUID
	ActorID   uuid.UUID
	SessionID uuid.UUID
	ReviewID  uuid.UUID
	After     float64
	Reason    string
	IP        *string
}

// ApplyCalibration records an adjusted_rating for a review and appends the
// before/after/reason to the session's decisions_json.  The original
// final_rating is NEVER mutated — adjustments live only in adjusted_rating, and
// the audit/decisions record preserves the change history.
//
// Only a review that has reached secondary_submitted may be calibrated, and a
// confirmed (read-only) review cannot be adjusted.
func (s *Service) ApplyCalibration(ctx context.Context, in ApplyCalibrationInput) (*Review, error) {
	var updated Review
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Load and lock the session.
		var sess CalibrationSession
		if err := tx.Raw(
			`SELECT id, tenant_id, cycle_id, name, facilitator_user_id, status,
			        decisions_json, created_at, updated_at
			 FROM calibration_sessions WHERE id = ? AND tenant_id = ? LIMIT 1
			 FOR UPDATE`,
			in.SessionID, in.TenantID,
		).Scan(&sess).Error; err != nil {
			return fmt.Errorf("evaluation: apply calibration load session: %w", err)
		}
		if sess.ID == uuid.Nil {
			return ErrNotFound
		}
		if sess.Status != CalibrationOpen {
			return fmt.Errorf("%w: calibration session is closed", ErrInvalidTransition)
		}

		// Load and lock the review.
		review, err := loadReviewForUpdate(tx, in.TenantID, in.ReviewID)
		if err != nil {
			return err
		}
		if review.Status == StatusConfirmed {
			return ErrConfirmed
		}
		if review.Status != StatusSecondarySubmitted && review.Status != StatusCalibrated {
			return fmt.Errorf("%w: review status %q cannot be calibrated", ErrInvalidTransition, review.Status)
		}

		before := 0.0
		if review.FinalRating != nil {
			before = *review.FinalRating
		}

		// Apply adjusted_rating + move status to calibrated.  final_rating is
		// left untouched (immutability of the original score).
		res := tx.Exec(
			`UPDATE reviews
			 SET adjusted_rating = ?, status = ?, updated_at = now()
			 WHERE id = ? AND tenant_id = ?`,
			in.After, StatusCalibrated, in.ReviewID, in.TenantID,
		)
		if res.Error != nil {
			return fmt.Errorf("evaluation: apply calibration update review: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}

		// Append to decisions_json (history of adjustments).
		var decisions []CalibrationDecision
		if len(sess.DecisionsJSON) > 0 {
			if err := json.Unmarshal(sess.DecisionsJSON, &decisions); err != nil {
				return fmt.Errorf("evaluation: apply calibration parse decisions: %w", err)
			}
		}
		decisions = append(decisions, CalibrationDecision{
			ReviewID: in.ReviewID.String(),
			Before:   before,
			After:    in.After,
			Reason:   in.Reason,
		})
		newDecisions, err := json.Marshal(decisions)
		if err != nil {
			return fmt.Errorf("evaluation: apply calibration marshal decisions: %w", err)
		}
		if err := tx.Exec(
			`UPDATE calibration_sessions
			 SET decisions_json = ?::jsonb, updated_at = now()
			 WHERE id = ? AND tenant_id = ?`,
			newDecisions, in.SessionID, in.TenantID,
		).Error; err != nil {
			return fmt.Errorf("evaluation: apply calibration update session: %w", err)
		}

		if err := tx.Raw(reviewSelectByID, in.ReviewID, in.TenantID).Scan(&updated).Error; err != nil {
			return fmt.Errorf("evaluation: apply calibration re-read: %w", err)
		}

		// Audit: opaque review ID only — reason text is NOT included.
		idStr := in.ReviewID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "review.calibrated",
			ResourceType: "review",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &updated, nil
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// loadReviewForUpdate loads a review row FOR UPDATE (TOCTOU-safe) within the tx.
func loadReviewForUpdate(tx *gorm.DB, tenantID, id uuid.UUID) (*Review, error) {
	var review Review
	if err := tx.Raw(
		`SELECT id, tenant_id, cycle_id, template_id, employee_id,
		        primary_reviewer_id, secondary_reviewer_id, status,
		        final_rating, adjusted_rating, confirmed_at, created_at, updated_at
		 FROM reviews WHERE id = ? AND tenant_id = ? LIMIT 1
		 FOR UPDATE`,
		id, tenantID,
	).Scan(&review).Error; err != nil {
		return nil, fmt.Errorf("evaluation: load review for update: %w", err)
	}
	if review.ID == uuid.Nil {
		return nil, ErrNotFound
	}
	return &review, nil
}

// requireEmployee verifies an employee exists in the tenant (multi-layer defence
// on top of the composite FK).
func requireEmployee(tx *gorm.DB, tenantID, employeeID uuid.UUID) error {
	var cnt int64
	if err := tx.Raw(
		`SELECT COUNT(1) FROM employees WHERE id = ? AND tenant_id = ?`,
		employeeID, tenantID,
	).Scan(&cnt).Error; err != nil {
		return fmt.Errorf("evaluation: verify employee: %w", err)
	}
	if cnt == 0 {
		return ErrNotFound
	}
	return nil
}

// requireTemplate verifies a template exists in the tenant.
func requireTemplate(tx *gorm.DB, tenantID, templateID uuid.UUID) error {
	var cnt int64
	if err := tx.Raw(
		`SELECT COUNT(1) FROM review_templates WHERE id = ? AND tenant_id = ?`,
		templateID, tenantID,
	).Scan(&cnt).Error; err != nil {
		return fmt.Errorf("evaluation: verify template: %w", err)
	}
	if cnt == 0 {
		return ErrNotFound
	}
	return nil
}

// isStageLocked reports whether a stage's entries are locked given the review's
// current status (a stage locks once the review has been submitted past it).
func isStageLocked(status, stage string) bool {
	switch stage {
	case StageSelf:
		// self entries lock once self has been submitted.
		return status == StatusPrimarySubmitted ||
			status == StatusSecondarySubmitted ||
			status == StatusCalibrated ||
			status == StatusConfirmed ||
			status == StatusSelfSubmitted
	case StagePrimary:
		return status == StatusSecondarySubmitted ||
			status == StatusCalibrated ||
			status == StatusConfirmed ||
			status == StatusPrimarySubmitted
	case StageSecondary:
		return status == StatusCalibrated ||
			status == StatusConfirmed ||
			status == StatusSecondarySubmitted
	default:
		// 360 entries are written via Submit360Response and not gated here.
		return false
	}
}

// stageItemsComplete reports whether every template item has a non-null score
// for the given stage on this review.  Completeness is config-driven: the set of
// required items comes from the template's items_json.
func stageItemsComplete(tx *gorm.DB, tenantID uuid.UUID, review *Review, stage string) (bool, error) {
	var tmpl Template
	if err := tx.Raw(
		`SELECT id, tenant_id, items_json FROM review_templates
		 WHERE id = ? AND tenant_id = ? LIMIT 1`,
		review.TemplateID, tenantID,
	).Scan(&tmpl).Error; err != nil {
		return false, fmt.Errorf("evaluation: stage complete fetch template: %w", err)
	}
	if tmpl.ID == uuid.Nil {
		return false, ErrNotFound
	}

	var items []templateItem
	if len(tmpl.ItemsJSON) > 0 {
		if err := json.Unmarshal(tmpl.ItemsJSON, &items); err != nil {
			return false, fmt.Errorf("evaluation: stage complete parse items_json: %w", err)
		}
	}
	if len(items) == 0 {
		// No items defined — vacuously complete.
		return true, nil
	}

	// Load answered item keys (non-null score) for this stage.
	var answered []struct {
		ItemKey string `gorm:"column:item_key"`
	}
	if err := tx.Raw(
		`SELECT item_key FROM review_entries
		 WHERE review_id = ? AND tenant_id = ? AND stage = ? AND score IS NOT NULL`,
		review.ID, tenantID, stage,
	).Scan(&answered).Error; err != nil {
		return false, fmt.Errorf("evaluation: stage complete load entries: %w", err)
	}
	have := make(map[string]bool, len(answered))
	for _, a := range answered {
		have[a.ItemKey] = true
	}
	for _, it := range items {
		if !have[it.ItemKey] {
			return false, nil
		}
	}
	return true, nil
}

// defaultJSON returns raw when it is non-empty/non-null, otherwise the fallback.
func defaultJSON(raw []byte, fallback string) []byte {
	if len(raw) == 0 || string(raw) == "null" {
		return []byte(fallback)
	}
	return raw
}
