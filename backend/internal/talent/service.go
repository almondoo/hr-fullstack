package talent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/platform/audit"
	platformauth "github.com/your-org/hr-saas/internal/platform/auth"
	"github.com/your-org/hr-saas/internal/platform/crypto"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// Sentinel errors.
var (
	ErrNotFound          = errors.New("talent: not found")
	ErrInvalidTransition = errors.New("talent: invalid status transition")
	ErrForbidden         = errors.New("talent: permission denied")
	ErrInvalidLevel      = errors.New("talent: skill level out of range")
	ErrValidation        = errors.New("talent: invalid input")
)

// Permission strings (defence-in-depth re-checks).
const (
	permReadSensitive = "talent:read_sensitive"
	permFreeText      = "survey:read_freetext"
)

// allowedSimTransitions defines legal placement-simulation status moves.
// draft → applied | discarded are terminal.
var allowedSimTransitions = map[string]map[string]bool{
	SimStatusDraft: {
		SimStatusApplied:   true,
		SimStatusDiscarded: true,
	},
}

// allowedSurveyTransitions defines legal pulse-survey status moves.
var allowedSurveyTransitions = map[string]map[string]bool{
	SurveyStatusDraft: {
		SurveyStatusOpen: true,
	},
	SurveyStatusOpen: {
		SurveyStatusClosed: true,
	},
}

func isSimTransitionAllowed(current, next string) bool {
	if nexts, ok := allowedSimTransitions[current]; ok {
		return nexts[next]
	}
	return false
}

func isSurveyTransitionAllowed(current, next string) bool {
	if nexts, ok := allowedSurveyTransitions[current]; ok {
		return nexts[next]
	}
	return false
}

// skillLevels is the decoded shape of skills.levels_json used to validate the
// level assigned in employee_skills.  Level definitions are tenant
// configuration — not hard-coded thresholds.
type skillLevels struct {
	Min *int `json:"min"`
	Max *int `json:"max"`
}

// Service provides business logic for the talent-management domain.
type Service struct {
	tdb *tenantdb.TenantDB
}

// NewService constructs a Service.
func NewService(tdb *tenantdb.TenantDB) *Service {
	return &Service{tdb: tdb}
}

// ---------------------------------------------------------------------------
// Skill master — TM-020
// ---------------------------------------------------------------------------

// CreateSkillInput holds fields for creating a skill master entry.
type CreateSkillInput struct {
	TenantID   uuid.UUID
	ActorID    uuid.UUID
	Category   string
	Name       string
	LevelsJSON []byte
	IP         *string
}

// CreateSkill creates a new skill master entry.
func (s *Service) CreateSkill(ctx context.Context, in CreateSkillInput) (*Skill, error) {
	if in.Name == "" {
		return nil, fmt.Errorf("%w: name required", ErrValidation)
	}
	levels := in.LevelsJSON
	if len(levels) == 0 || string(levels) == "null" {
		levels = []byte(`{}`)
	}
	sk := Skill{
		ID:         uuid.New(),
		TenantID:   in.TenantID,
		Category:   in.Category,
		Name:       in.Name,
		LevelsJSON: levels,
		Active:     true,
	}
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		if err := tx.Exec(
			`INSERT INTO skills (id, tenant_id, category, name, levels_json, active)
			 VALUES (?, ?, ?, ?, ?::jsonb, ?)`,
			sk.ID, sk.TenantID, sk.Category, sk.Name, sk.LevelsJSON, sk.Active,
		).Error; err != nil {
			return fmt.Errorf("talent: create skill insert: %w", err)
		}
		idStr := sk.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "skill.created",
			ResourceType: "skill",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &sk, nil
}

// ListSkills returns active skills for a tenant, optionally filtered by category.
func (s *Service) ListSkills(ctx context.Context, tenantID uuid.UUID, category string) ([]Skill, error) {
	var skills []Skill
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		q := `SELECT id, tenant_id, category, name, levels_json, active, created_at, updated_at
		      FROM skills WHERE tenant_id = ? AND active = true`
		args := []any{tenantID}
		if category != "" {
			q += ` AND category = ?`
			args = append(args, category)
		}
		q += ` ORDER BY category, name`
		return tx.Raw(q, args...).Scan(&skills).Error
	})
	if err != nil {
		return nil, err
	}
	return skills, nil
}

// ---------------------------------------------------------------------------
// Employee skills (skill map) — TM-020
// ---------------------------------------------------------------------------

// AssignSkillInput holds fields for assigning/updating an employee skill.
type AssignSkillInput struct {
	TenantID   uuid.UUID
	ActorID    uuid.UUID
	EmployeeID uuid.UUID
	SkillID    uuid.UUID
	Level      int
	AcquiredOn *time.Time
	ExpiresOn  *time.Time
	IP         *string
}

// AssignSkill upserts an employee's skill.  The level is validated against the
// skill's levels_json (min/max) — tenant configuration, not hard-coded.
// Both employee and skill must belong to the same tenant (enforced by composite
// FK and explicit checks).
func (s *Service) AssignSkill(ctx context.Context, in AssignSkillInput) (*EmployeeSkill, error) {
	var es EmployeeSkill
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Verify employee belongs to this tenant.
		var empCount int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM employees WHERE id = ? AND tenant_id = ?`,
			in.EmployeeID, in.TenantID,
		).Scan(&empCount).Error; err != nil {
			return fmt.Errorf("talent: assign skill verify employee: %w", err)
		}
		if empCount == 0 {
			return ErrNotFound
		}

		// Fetch the skill and validate level against levels_json bounds.
		var skill Skill
		if err := tx.Raw(
			`SELECT id, tenant_id, levels_json FROM skills
			 WHERE id = ? AND tenant_id = ? AND active = true LIMIT 1`,
			in.SkillID, in.TenantID,
		).Scan(&skill).Error; err != nil {
			return fmt.Errorf("talent: assign skill fetch skill: %w", err)
		}
		if skill.ID == uuid.Nil {
			return ErrNotFound
		}
		if err := validateLevel(skill.LevelsJSON, in.Level); err != nil {
			return err
		}

		id := uuid.New()
		if err := tx.Exec(
			`INSERT INTO employee_skills
			   (id, tenant_id, employee_id, skill_id, level, acquired_on, expires_on)
			 VALUES (?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT (employee_id, skill_id, tenant_id) DO UPDATE
			   SET level       = EXCLUDED.level,
			       acquired_on = EXCLUDED.acquired_on,
			       expires_on  = EXCLUDED.expires_on,
			       updated_at  = now()`,
			id, in.TenantID, in.EmployeeID, in.SkillID, in.Level, in.AcquiredOn, in.ExpiresOn,
		).Error; err != nil {
			return fmt.Errorf("talent: assign skill upsert: %w", err)
		}

		if err := tx.Raw(
			`SELECT id, tenant_id, employee_id, skill_id, level, acquired_on, expires_on,
			        created_at, updated_at
			 FROM employee_skills
			 WHERE employee_id = ? AND skill_id = ? AND tenant_id = ? LIMIT 1`,
			in.EmployeeID, in.SkillID, in.TenantID,
		).Scan(&es).Error; err != nil {
			return fmt.Errorf("talent: assign skill re-read: %w", err)
		}

		idStr := es.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "employee_skill.assigned",
			ResourceType: "employee_skill",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &es, nil
}

// validateLevel checks that level is within the skill's configured [min,max].
// When min/max are absent in levels_json, only a non-negative check applies.
func validateLevel(levelsJSON []byte, level int) error {
	if level < 0 {
		return fmt.Errorf("%w: level must be >= 0", ErrInvalidLevel)
	}
	if len(levelsJSON) == 0 {
		return nil
	}
	var sl skillLevels
	if err := json.Unmarshal(levelsJSON, &sl); err != nil {
		// Malformed config should not block assignment with a non-negative level;
		// treat as "no bounds defined".
		return nil
	}
	if sl.Min != nil && level < *sl.Min {
		return fmt.Errorf("%w: level %d below min %d", ErrInvalidLevel, level, *sl.Min)
	}
	if sl.Max != nil && level > *sl.Max {
		return fmt.Errorf("%w: level %d above max %d", ErrInvalidLevel, level, *sl.Max)
	}
	return nil
}

// ListEmployeeSkills returns all skills held by an employee.
func (s *Service) ListEmployeeSkills(ctx context.Context, tenantID, employeeID uuid.UUID) ([]EmployeeSkill, error) {
	var out []EmployeeSkill
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, employee_id, skill_id, level, acquired_on, expires_on,
			        created_at, updated_at
			 FROM employee_skills
			 WHERE tenant_id = ? AND employee_id = ?
			 ORDER BY skill_id`,
			tenantID, employeeID,
		).Scan(&out).Error
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SearchSkillHolders returns employees holding skillID at >= minLevel.
func (s *Service) SearchSkillHolders(ctx context.Context, tenantID, skillID uuid.UUID, minLevel int) ([]SkillHolder, error) {
	var out []SkillHolder
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT es.employee_id AS employee_id, e.employee_code AS employee_code, es.level AS level
			 FROM employee_skills es
			 JOIN employees e ON e.id = es.employee_id AND e.tenant_id = es.tenant_id
			 WHERE es.tenant_id = ? AND es.skill_id = ? AND es.level >= ?
			 ORDER BY es.level DESC, e.employee_code`,
			tenantID, skillID, minLevel,
		).Scan(&out).Error
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SkillMatrix returns a department × skill aggregation (holder count + avg level).
func (s *Service) SkillMatrix(ctx context.Context, tenantID uuid.UUID) ([]SkillMatrixCell, error) {
	var out []SkillMatrixCell
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT e.department_id AS department_id,
			        es.skill_id     AS skill_id,
			        sk.name         AS skill_name,
			        COUNT(1)        AS holder_count,
			        AVG(es.level)   AS avg_level
			 FROM employee_skills es
			 JOIN employees e ON e.id = es.employee_id AND e.tenant_id = es.tenant_id
			 JOIN skills sk ON sk.id = es.skill_id AND sk.tenant_id = es.tenant_id
			 WHERE es.tenant_id = ?
			 GROUP BY e.department_id, es.skill_id, sk.name
			 ORDER BY es.skill_id`,
			tenantID,
		).Scan(&out).Error
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Certifications — TM-020
// ---------------------------------------------------------------------------

// AddCertificationInput holds fields for adding a certification.
type AddCertificationInput struct {
	TenantID        uuid.UUID
	ActorID         uuid.UUID
	EmployeeID      uuid.UUID
	Name            string
	Issuer          string
	AcquiredOn      *time.Time
	ExpiresOn       *time.Time
	RenewalRequired bool
	IP              *string
}

// AddCertification records a certification for an employee.
func (s *Service) AddCertification(ctx context.Context, in AddCertificationInput) (*Certification, error) {
	if in.Name == "" {
		return nil, fmt.Errorf("%w: name required", ErrValidation)
	}
	cert := Certification{
		ID:              uuid.New(),
		TenantID:        in.TenantID,
		EmployeeID:      in.EmployeeID,
		Name:            in.Name,
		Issuer:          in.Issuer,
		AcquiredOn:      in.AcquiredOn,
		ExpiresOn:       in.ExpiresOn,
		RenewalRequired: in.RenewalRequired,
	}
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		var empCount int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM employees WHERE id = ? AND tenant_id = ?`,
			in.EmployeeID, in.TenantID,
		).Scan(&empCount).Error; err != nil {
			return fmt.Errorf("talent: add certification verify employee: %w", err)
		}
		if empCount == 0 {
			return ErrNotFound
		}
		if err := tx.Exec(
			`INSERT INTO employee_certifications
			   (id, tenant_id, employee_id, name, issuer, acquired_on, expires_on, renewal_required)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			cert.ID, cert.TenantID, cert.EmployeeID, cert.Name, cert.Issuer,
			cert.AcquiredOn, cert.ExpiresOn, cert.RenewalRequired,
		).Error; err != nil {
			return fmt.Errorf("talent: add certification insert: %w", err)
		}
		idStr := cert.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "certification.added",
			ResourceType: "employee_certification",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &cert, nil
}

// ListCertifications returns an employee's certifications.
func (s *Service) ListCertifications(ctx context.Context, tenantID, employeeID uuid.UUID) ([]Certification, error) {
	var out []Certification
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, employee_id, name, issuer, acquired_on, expires_on,
			        renewal_required, created_at, updated_at
			 FROM employee_certifications
			 WHERE tenant_id = ? AND employee_id = ?
			 ORDER BY expires_on NULLS LAST, name`,
			tenantID, employeeID,
		).Scan(&out).Error
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ExpiringCertifications returns certifications expiring on or before
// (now + withinDays).  withinDays is supplied by configuration (legal/privacy
// policy), not hard-coded.  Notification dispatch is a future hook.
func (s *Service) ExpiringCertifications(ctx context.Context, tenantID uuid.UUID, withinDays int) ([]Certification, error) {
	if withinDays < 0 {
		return nil, fmt.Errorf("%w: withinDays must be >= 0", ErrValidation)
	}
	cutoff := time.Now().AddDate(0, 0, withinDays)
	var out []Certification
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, employee_id, name, issuer, acquired_on, expires_on,
			        renewal_required, created_at, updated_at
			 FROM employee_certifications
			 WHERE tenant_id = ? AND expires_on IS NOT NULL AND expires_on <= ?
			 ORDER BY expires_on`,
			tenantID, cutoff,
		).Scan(&out).Error
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ExpiringSkills returns employee skills expiring on or before (now + withinDays).
func (s *Service) ExpiringSkills(ctx context.Context, tenantID uuid.UUID, withinDays int) ([]EmployeeSkill, error) {
	if withinDays < 0 {
		return nil, fmt.Errorf("%w: withinDays must be >= 0", ErrValidation)
	}
	cutoff := time.Now().AddDate(0, 0, withinDays)
	var out []EmployeeSkill
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, employee_id, skill_id, level, acquired_on, expires_on,
			        created_at, updated_at
			 FROM employee_skills
			 WHERE tenant_id = ? AND expires_on IS NOT NULL AND expires_on <= ?
			 ORDER BY expires_on`,
			tenantID, cutoff,
		).Scan(&out).Error
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Integrated profile — TM-020
// ---------------------------------------------------------------------------

// GetProfileInput holds parameters for the integrated profile.
type GetProfileInput struct {
	TenantID   uuid.UUID
	ActorID    uuid.UUID
	EmployeeID uuid.UUID
}

// GetIntegratedProfile aggregates an employee's basic info, assignment history,
// and held skills into one view.  Compensation/grade fields are masked unless
// the viewer holds talent:read_sensitive (verified in the service layer for
// defence-in-depth).  Skills are batch-loaded (no N+1).
func (s *Service) GetIntegratedProfile(ctx context.Context, in GetProfileInput) (*IntegratedProfile, error) {
	var profile IntegratedProfile
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Determine sensitive access (defence-in-depth — not relying solely on
		// the HTTP middleware).
		perms, err := platformauth.LoadUserPermissions(tx, in.TenantID, in.ActorID)
		if err != nil {
			return fmt.Errorf("talent: profile load permissions: %w", err)
		}
		canReadSensitive := platformauth.HasPermission(perms, permReadSensitive)

		// Basic employee info.
		var emp struct {
			ID           uuid.UUID  `gorm:"column:id"`
			EmployeeCode string     `gorm:"column:employee_code"`
			LastName     string     `gorm:"column:last_name"`
			FirstName    string     `gorm:"column:first_name"`
			Status       string     `gorm:"column:status"`
			DepartmentID *uuid.UUID `gorm:"column:department_id"`
		}
		if err := tx.Raw(
			`SELECT id, employee_code, last_name, first_name, status, department_id
			 FROM employees WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.EmployeeID, in.TenantID,
		).Scan(&emp).Error; err != nil {
			return fmt.Errorf("talent: profile read employee: %w", err)
		}
		if emp.ID == uuid.Nil {
			return ErrNotFound
		}

		profile.EmployeeID = emp.ID
		profile.EmployeeCode = emp.EmployeeCode
		profile.LastName = emp.LastName
		profile.FirstName = emp.FirstName
		profile.Status = emp.Status
		profile.DepartmentID = emp.DepartmentID
		profile.SensitiveMasked = !canReadSensitive

		// Assignment history (batch single query).
		var assignments []AssignmentSummary
		if err := tx.Raw(
			`SELECT id, department_id, position, grade, effective_from, effective_to
			 FROM employee_assignments
			 WHERE tenant_id = ? AND employee_id = ?
			 ORDER BY effective_from DESC`,
			in.TenantID, in.EmployeeID,
		).Scan(&assignments).Error; err != nil {
			return fmt.Errorf("talent: profile read assignments: %w", err)
		}
		// Mask grade (compensation-related) when the viewer lacks sensitive access.
		if !canReadSensitive {
			for i := range assignments {
				assignments[i].Grade = nil
			}
		}
		profile.Assignments = assignments

		// Held skills (batch single query — no N+1).
		var skills []EmployeeSkill
		if err := tx.Raw(
			`SELECT id, tenant_id, employee_id, skill_id, level, acquired_on, expires_on,
			        created_at, updated_at
			 FROM employee_skills
			 WHERE tenant_id = ? AND employee_id = ?
			 ORDER BY skill_id`,
			in.TenantID, in.EmployeeID,
		).Scan(&skills).Error; err != nil {
			return fmt.Errorf("talent: profile read skills: %w", err)
		}
		profile.Skills = skills

		// Audit the profile read (metadata only — no PII).
		idStr := in.EmployeeID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "talent_profile.read",
			ResourceType: "employee",
			ResourceID:   &idStr,
			IP:           nil,
		})
	})
	if err != nil {
		return nil, err
	}
	return &profile, nil
}

// ---------------------------------------------------------------------------
// Org tree (組織図ビュー) — TM-021
// ---------------------------------------------------------------------------

// GetOrgTree builds the department hierarchy with current employee head counts.
func (s *Service) GetOrgTree(ctx context.Context, tenantID uuid.UUID) ([]*OrgNode, error) {
	var roots []*OrgNode
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		type deptRow struct {
			ID       uuid.UUID  `gorm:"column:id"`
			ParentID *uuid.UUID `gorm:"column:parent_id"`
			Name     string     `gorm:"column:name"`
			Code     string     `gorm:"column:code"`
		}
		var depts []deptRow
		if err := tx.Raw(
			`SELECT id, parent_id, name, code FROM departments
			 WHERE tenant_id = ? ORDER BY code`,
			tenantID,
		).Scan(&depts).Error; err != nil {
			return fmt.Errorf("talent: org tree read departments: %w", err)
		}

		// Head counts grouped by department (single query — no N+1).
		type countRow struct {
			DepartmentID uuid.UUID `gorm:"column:department_id"`
			Cnt          int       `gorm:"column:cnt"`
		}
		var counts []countRow
		if err := tx.Raw(
			`SELECT department_id, COUNT(1) AS cnt FROM employees
			 WHERE tenant_id = ? AND department_id IS NOT NULL
			   AND status NOT IN ('terminated', 'left')
			 GROUP BY department_id`,
			tenantID,
		).Scan(&counts).Error; err != nil {
			return fmt.Errorf("talent: org tree read counts: %w", err)
		}
		countByDept := make(map[uuid.UUID]int, len(counts))
		for _, c := range counts {
			countByDept[c.DepartmentID] = c.Cnt
		}

		// Build nodes and link children.
		nodes := make(map[uuid.UUID]*OrgNode, len(depts))
		for _, d := range depts {
			nodes[d.ID] = &OrgNode{
				DepartmentID:  d.ID,
				ParentID:      d.ParentID,
				Name:          d.Name,
				Code:          d.Code,
				EmployeeCount: countByDept[d.ID],
				Children:      []*OrgNode{},
			}
		}
		for _, d := range depts {
			node := nodes[d.ID]
			// A parent in a different tenant cannot appear here (RLS scopes the
			// SELECT to this tenant); treat unknown parents as roots.
			if d.ParentID != nil {
				if parent, ok := nodes[*d.ParentID]; ok {
					parent.Children = append(parent.Children, node)
					continue
				}
			}
			roots = append(roots, node)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return roots, nil
}

// ---------------------------------------------------------------------------
// Placement simulation — TM-021
// ---------------------------------------------------------------------------

// CreateSimulationInput holds fields for creating a placement simulation.
type CreateSimulationInput struct {
	TenantID uuid.UUID
	ActorID  uuid.UUID
	Name     string
	IP       *string
}

// CreateSimulation creates a draft placement simulation header.
func (s *Service) CreateSimulation(ctx context.Context, in CreateSimulationInput) (*PlacementSimulation, error) {
	if in.Name == "" {
		return nil, fmt.Errorf("%w: name required", ErrValidation)
	}
	sim := PlacementSimulation{
		ID:              uuid.New(),
		TenantID:        in.TenantID,
		Name:            in.Name,
		Status:          SimStatusDraft,
		CreatedByUserID: &in.ActorID,
	}
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		if err := tx.Exec(
			`INSERT INTO placement_simulations (id, tenant_id, name, status, created_by_user_id)
			 VALUES (?, ?, ?, ?, ?)`,
			sim.ID, sim.TenantID, sim.Name, sim.Status, sim.CreatedByUserID,
		).Error; err != nil {
			return fmt.Errorf("talent: create simulation insert: %w", err)
		}
		idStr := sim.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "placement_simulation.created",
			ResourceType: "placement_simulation",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &sim, nil
}

// AddSimulationItemInput holds fields for adding a move item to a simulation.
type AddSimulationItemInput struct {
	TenantID           uuid.UUID
	ActorID            uuid.UUID
	SimulationID       uuid.UUID
	EmployeeID         uuid.UUID
	TargetDepartmentID *uuid.UUID
	TargetPosition     *string
	TargetGrade        *string
	EffectiveFrom      time.Time
	Reason             *string
	IP                 *string
}

// AddSimulationItem adds a proposed move to a draft simulation.
// The simulation must be in draft status.  Adding an item NEVER touches the
// real org (departments / employee_assignments).
func (s *Service) AddSimulationItem(ctx context.Context, in AddSimulationItemInput) (*PlacementSimulationItem, error) {
	item := PlacementSimulationItem{
		ID:                 uuid.New(),
		TenantID:           in.TenantID,
		SimulationID:       in.SimulationID,
		EmployeeID:         in.EmployeeID,
		TargetDepartmentID: in.TargetDepartmentID,
		TargetPosition:     in.TargetPosition,
		TargetGrade:        in.TargetGrade,
		EffectiveFrom:      in.EffectiveFrom,
		Reason:             in.Reason,
	}
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Verify the simulation exists, is in this tenant, and is still a draft.
		var sim struct {
			Status string `gorm:"column:status"`
		}
		if err := tx.Raw(
			`SELECT status FROM placement_simulations
			 WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.SimulationID, in.TenantID,
		).Scan(&sim).Error; err != nil {
			return fmt.Errorf("talent: add sim item read simulation: %w", err)
		}
		if sim.Status == "" {
			return ErrNotFound
		}
		if sim.Status != SimStatusDraft {
			return fmt.Errorf("%w: simulation status %s is not draft", ErrInvalidTransition, sim.Status)
		}

		// Verify employee belongs to this tenant.
		var empCount int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM employees WHERE id = ? AND tenant_id = ?`,
			in.EmployeeID, in.TenantID,
		).Scan(&empCount).Error; err != nil {
			return fmt.Errorf("talent: add sim item verify employee: %w", err)
		}
		if empCount == 0 {
			return ErrNotFound
		}

		// Verify target department belongs to this tenant when supplied.
		if in.TargetDepartmentID != nil {
			var deptCount int64
			if err := tx.Raw(
				`SELECT COUNT(1) FROM departments WHERE id = ? AND tenant_id = ?`,
				*in.TargetDepartmentID, in.TenantID,
			).Scan(&deptCount).Error; err != nil {
				return fmt.Errorf("talent: add sim item verify department: %w", err)
			}
			if deptCount == 0 {
				return ErrNotFound
			}
		}

		if err := tx.Exec(
			`INSERT INTO placement_simulation_items
			   (id, tenant_id, simulation_id, employee_id, target_department_id,
			    target_position, target_grade, effective_from, reason)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			item.ID, item.TenantID, item.SimulationID, item.EmployeeID,
			item.TargetDepartmentID, item.TargetPosition, item.TargetGrade,
			item.EffectiveFrom, item.Reason,
		).Error; err != nil {
			return fmt.Errorf("talent: add sim item insert: %w", err)
		}

		idStr := item.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "placement_simulation_item.added",
			ResourceType: "placement_simulation_item",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &item, nil
}

// ListSimulationItems returns the move items for a simulation.
func (s *Service) ListSimulationItems(ctx context.Context, tenantID, simulationID uuid.UUID) ([]PlacementSimulationItem, error) {
	var out []PlacementSimulationItem
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		// Verify the simulation belongs to this tenant first.
		var cnt int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM placement_simulations WHERE id = ? AND tenant_id = ?`,
			simulationID, tenantID,
		).Scan(&cnt).Error; err != nil {
			return fmt.Errorf("talent: list sim items verify: %w", err)
		}
		if cnt == 0 {
			return ErrNotFound
		}
		return tx.Raw(
			`SELECT id, tenant_id, simulation_id, employee_id, target_department_id,
			        target_position, target_grade, effective_from, reason, created_at
			 FROM placement_simulation_items
			 WHERE tenant_id = ? AND simulation_id = ?
			 ORDER BY created_at`,
			tenantID, simulationID,
		).Scan(&out).Error
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ApplySimulationInput holds fields for applying a simulation.
type ApplySimulationInput struct {
	TenantID     uuid.UUID
	ActorID      uuid.UUID
	SimulationID uuid.UUID
	IP           *string
}

// ApplySimulation transitions a draft simulation to "applied" and maps each of
// its items to an employee_assignments (発令履歴) row — all within a single
// transaction.  Double-apply is prevented: only a draft can be applied
// (ErrInvalidTransition otherwise).
func (s *Service) ApplySimulation(ctx context.Context, in ApplySimulationInput) (int, error) {
	var applied int
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Lock the header row to avoid concurrent double-apply (TOCTOU).
		var sim struct {
			Status string `gorm:"column:status"`
		}
		if err := tx.Raw(
			`SELECT status FROM placement_simulations
			 WHERE id = ? AND tenant_id = ? LIMIT 1 FOR UPDATE`,
			in.SimulationID, in.TenantID,
		).Scan(&sim).Error; err != nil {
			return fmt.Errorf("talent: apply simulation lock: %w", err)
		}
		if sim.Status == "" {
			return ErrNotFound
		}
		if !isSimTransitionAllowed(sim.Status, SimStatusApplied) {
			return fmt.Errorf("%w: simulation status %s → applied", ErrInvalidTransition, sim.Status)
		}

		// Read items.
		var items []PlacementSimulationItem
		if err := tx.Raw(
			`SELECT id, tenant_id, simulation_id, employee_id, target_department_id,
			        target_position, target_grade, effective_from, reason, created_at
			 FROM placement_simulation_items
			 WHERE tenant_id = ? AND simulation_id = ?
			 ORDER BY created_at`,
			in.TenantID, in.SimulationID,
		).Scan(&items).Error; err != nil {
			return fmt.Errorf("talent: apply simulation read items: %w", err)
		}

		// Map each item to a new employee_assignments row.  This is the only
		// point at which the real org is mutated.
		for _, it := range items {
			assignmentID := uuid.New()
			reason := "placement simulation apply"
			if it.Reason != nil && *it.Reason != "" {
				reason = *it.Reason
			}
			if err := tx.Exec(
				`INSERT INTO employee_assignments
				   (id, tenant_id, employee_id, department_id, position, grade,
				    effective_from, reason)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				assignmentID, in.TenantID, it.EmployeeID, it.TargetDepartmentID,
				it.TargetPosition, it.TargetGrade, it.EffectiveFrom, reason,
			).Error; err != nil {
				return fmt.Errorf("talent: apply simulation insert assignment: %w", err)
			}
			applied++
		}

		// Transition the header to applied.
		res := tx.Exec(
			`UPDATE placement_simulations
			 SET status = ?, applied_at = now(), updated_at = now()
			 WHERE id = ? AND tenant_id = ? AND status = ?`,
			SimStatusApplied, in.SimulationID, in.TenantID, SimStatusDraft,
		)
		if res.Error != nil {
			return fmt.Errorf("talent: apply simulation update header: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			// Another transaction applied/discarded it first.
			return fmt.Errorf("%w: simulation already finalised", ErrInvalidTransition)
		}

		idStr := in.SimulationID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "placement_simulation.applied",
			ResourceType: "placement_simulation",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return 0, err
	}
	return applied, nil
}

// DiscardSimulationInput holds fields for discarding a simulation.
type DiscardSimulationInput struct {
	TenantID     uuid.UUID
	ActorID      uuid.UUID
	SimulationID uuid.UUID
	IP           *string
}

// DiscardSimulation transitions a draft simulation to "discarded" without
// touching the real org.
func (s *Service) DiscardSimulation(ctx context.Context, in DiscardSimulationInput) error {
	return s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		var sim struct {
			Status string `gorm:"column:status"`
		}
		if err := tx.Raw(
			`SELECT status FROM placement_simulations
			 WHERE id = ? AND tenant_id = ? LIMIT 1 FOR UPDATE`,
			in.SimulationID, in.TenantID,
		).Scan(&sim).Error; err != nil {
			return fmt.Errorf("talent: discard simulation lock: %w", err)
		}
		if sim.Status == "" {
			return ErrNotFound
		}
		if !isSimTransitionAllowed(sim.Status, SimStatusDiscarded) {
			return fmt.Errorf("%w: simulation status %s → discarded", ErrInvalidTransition, sim.Status)
		}
		res := tx.Exec(
			`UPDATE placement_simulations
			 SET status = ?, updated_at = now()
			 WHERE id = ? AND tenant_id = ? AND status = ?`,
			SimStatusDiscarded, in.SimulationID, in.TenantID, SimStatusDraft,
		)
		if res.Error != nil {
			return fmt.Errorf("talent: discard simulation update: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("%w: simulation already finalised", ErrInvalidTransition)
		}
		idStr := in.SimulationID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "placement_simulation.discarded",
			ResourceType: "placement_simulation",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
}

// ---------------------------------------------------------------------------
// Pulse surveys — TM-022
// ---------------------------------------------------------------------------

// CreateSurveyInput holds fields for creating a pulse survey.
type CreateSurveyInput struct {
	TenantID           uuid.UUID
	ActorID            uuid.UUID
	Title              string
	QuestionsJSON      []byte
	Anonymous          bool
	MinResponsesToShow int
	StartsOn           *time.Time
	EndsOn             *time.Time
	IP                 *string
}

// CreateSurvey creates a draft pulse survey.  MinResponsesToShow is the
// minimum-disclosure threshold (privacy configuration).
func (s *Service) CreateSurvey(ctx context.Context, in CreateSurveyInput) (*PulseSurvey, error) {
	if in.Title == "" {
		return nil, fmt.Errorf("%w: title required", ErrValidation)
	}
	if in.MinResponsesToShow < 1 {
		return nil, fmt.Errorf("%w: min_responses_to_show must be >= 1", ErrValidation)
	}
	questions := in.QuestionsJSON
	if len(questions) == 0 || string(questions) == "null" {
		questions = []byte(`[]`)
	}
	survey := PulseSurvey{
		ID:                 uuid.New(),
		TenantID:           in.TenantID,
		Title:              in.Title,
		QuestionsJSON:      questions,
		Anonymous:          in.Anonymous,
		MinResponsesToShow: in.MinResponsesToShow,
		StartsOn:           in.StartsOn,
		EndsOn:             in.EndsOn,
		Status:             SurveyStatusDraft,
	}
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		if err := tx.Exec(
			`INSERT INTO pulse_surveys
			   (id, tenant_id, title, questions_json, anonymous, min_responses_to_show,
			    starts_on, ends_on, status)
			 VALUES (?, ?, ?, ?::jsonb, ?, ?, ?, ?, ?)`,
			survey.ID, survey.TenantID, survey.Title, survey.QuestionsJSON,
			survey.Anonymous, survey.MinResponsesToShow, survey.StartsOn,
			survey.EndsOn, survey.Status,
		).Error; err != nil {
			return fmt.Errorf("talent: create survey insert: %w", err)
		}
		idStr := survey.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "pulse_survey.created",
			ResourceType: "pulse_survey",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &survey, nil
}

// SetSurveyStatusInput holds fields for transitioning a survey's status.
type SetSurveyStatusInput struct {
	TenantID uuid.UUID
	ActorID  uuid.UUID
	SurveyID uuid.UUID
	Status   string
	IP       *string
}

// SetSurveyStatus transitions a survey through draft → open → closed.
func (s *Service) SetSurveyStatus(ctx context.Context, in SetSurveyStatusInput) (*PulseSurvey, error) {
	var survey PulseSurvey
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		var current struct {
			Status string `gorm:"column:status"`
		}
		if err := tx.Raw(
			`SELECT status FROM pulse_surveys WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.SurveyID, in.TenantID,
		).Scan(&current).Error; err != nil {
			return fmt.Errorf("talent: set survey status read: %w", err)
		}
		if current.Status == "" {
			return ErrNotFound
		}
		if !isSurveyTransitionAllowed(current.Status, in.Status) {
			return fmt.Errorf("%w: survey %s → %s", ErrInvalidTransition, current.Status, in.Status)
		}
		res := tx.Exec(
			`UPDATE pulse_surveys SET status = ?, updated_at = now()
			 WHERE id = ? AND tenant_id = ?`,
			in.Status, in.SurveyID, in.TenantID,
		)
		if res.Error != nil {
			return fmt.Errorf("talent: set survey status update: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}
		if err := tx.Raw(
			`SELECT id, tenant_id, title, questions_json, anonymous, min_responses_to_show,
			        starts_on, ends_on, status, created_at, updated_at
			 FROM pulse_surveys WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.SurveyID, in.TenantID,
		).Scan(&survey).Error; err != nil {
			return fmt.Errorf("talent: set survey status re-read: %w", err)
		}
		idStr := in.SurveyID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "pulse_survey.status_updated",
			ResourceType: "pulse_survey",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &survey, nil
}

// SubmitResponseInput holds fields for submitting a survey response.
//
// FreeTextPlaintext contains the free-text answer in plaintext.  It is encrypted
// with AES-256-GCM before storage; the plaintext is NEVER persisted, logged, or
// written to audit records.  For anonymous surveys RespondentEmployeeID is
// forced to NULL so responses cannot be reverse-linked.
type SubmitResponseInput struct {
	TenantID    uuid.UUID
	ActorID     uuid.UUID
	SurveyID    uuid.UUID
	EmployeeID  *uuid.UUID
	AnswersJSON []byte
	// FreeTextPlaintext MUST NOT be logged, audited, or persisted as plaintext.
	FreeTextPlaintext []byte
	IP                *string
}

// SubmitResponse records a survey response.  The free-text answer is encrypted
// before storage.  Responses are only accepted while the survey is "open".
// For anonymous surveys the respondent is stored as NULL.
func (s *Service) SubmitResponse(ctx context.Context, in SubmitResponseInput) (*PulseSurveyResponse, error) {
	// Encrypt free-text BEFORE opening the transaction (fail-fast; plaintext
	// never appears in any error message).
	var freeTextEnc []byte
	if len(in.FreeTextPlaintext) > 0 {
		enc, err := crypto.Encrypt(in.FreeTextPlaintext)
		if err != nil {
			return nil, fmt.Errorf("talent: encrypt free_text: %w", err)
		}
		freeTextEnc = enc
	}

	answers := in.AnswersJSON
	if len(answers) == 0 || string(answers) == "null" {
		answers = []byte(`{}`)
	}

	resp := PulseSurveyResponse{
		ID:          uuid.New(),
		TenantID:    in.TenantID,
		SurveyID:    in.SurveyID,
		AnswersJSON: answers,
		FreeText:    freeTextEnc,
	}

	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Verify survey exists, is in this tenant, and is open.
		var survey struct {
			Status    string `gorm:"column:status"`
			Anonymous bool   `gorm:"column:anonymous"`
		}
		if err := tx.Raw(
			`SELECT status, anonymous FROM pulse_surveys
			 WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.SurveyID, in.TenantID,
		).Scan(&survey).Error; err != nil {
			return fmt.Errorf("talent: submit response read survey: %w", err)
		}
		if survey.Status == "" {
			return ErrNotFound
		}
		if survey.Status != SurveyStatusOpen {
			return fmt.Errorf("%w: survey not open (status %s)", ErrInvalidTransition, survey.Status)
		}

		// Anonymity enforcement: NEVER store the respondent for anonymous surveys.
		var respondent *uuid.UUID
		if !survey.Anonymous && in.EmployeeID != nil {
			// Verify the respondent employee belongs to this tenant.
			var empCount int64
			if err := tx.Raw(
				`SELECT COUNT(1) FROM employees WHERE id = ? AND tenant_id = ?`,
				*in.EmployeeID, in.TenantID,
			).Scan(&empCount).Error; err != nil {
				return fmt.Errorf("talent: submit response verify employee: %w", err)
			}
			if empCount == 0 {
				return ErrNotFound
			}
			respondent = in.EmployeeID
		}
		resp.RespondentEmployeeID = respondent

		if err := tx.Exec(
			`INSERT INTO pulse_survey_responses
			   (id, tenant_id, survey_id, respondent_employee_id, answers_json, free_text)
			 VALUES (?, ?, ?, ?, ?::jsonb, ?)`,
			resp.ID, resp.TenantID, resp.SurveyID, resp.RespondentEmployeeID,
			resp.AnswersJSON, resp.FreeText,
		).Error; err != nil {
			return fmt.Errorf("talent: submit response insert: %w", err)
		}

		// Audit: only the response ID (opaque) — never the respondent, free-text,
		// or any PII.
		idStr := resp.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "pulse_survey_response.submitted",
			ResourceType: "pulse_survey_response",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	// Never expose ciphertext to callers.
	resp.FreeText = nil
	return &resp, nil
}

// AggregateSurvey returns the survey response aggregation, honouring the
// minimum-disclosure threshold.  When the response count is below
// min_responses_to_show the answer summary is suppressed.  The respondent
// identity is never included for anonymous surveys (it is NULL in the DB), and
// free-text is never aggregated.
func (s *Service) AggregateSurvey(ctx context.Context, tenantID, surveyID uuid.UUID) (*SurveyAggregate, error) {
	agg := SurveyAggregate{SurveyID: surveyID, AnswerSummary: map[string]float64{}}
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		var survey struct {
			MinResponses int `gorm:"column:min_responses_to_show"`
			Found        int `gorm:"column:found"`
		}
		if err := tx.Raw(
			`SELECT min_responses_to_show, 1 AS found FROM pulse_surveys
			 WHERE id = ? AND tenant_id = ? LIMIT 1`,
			surveyID, tenantID,
		).Scan(&survey).Error; err != nil {
			return fmt.Errorf("talent: aggregate read survey: %w", err)
		}
		if survey.Found == 0 {
			return ErrNotFound
		}

		var count int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM pulse_survey_responses
			 WHERE tenant_id = ? AND survey_id = ?`,
			tenantID, surveyID,
		).Scan(&count).Error; err != nil {
			return fmt.Errorf("talent: aggregate count: %w", err)
		}
		agg.ResponseCount = int(count)

		if int(count) < survey.MinResponses {
			// Below the minimum-disclosure threshold — suppress the summary.
			agg.Suppressed = true
			return nil
		}

		// Numeric per-question aggregation only.  Free-text is NEVER aggregated
		// (it is encrypted and may contain sensitive content).
		type answerAgg struct {
			Key string  `gorm:"column:k"`
			Avg float64 `gorm:"column:avg_v"`
		}
		var rows []answerAgg
		if err := tx.Raw(
			`SELECT kv.key AS k, AVG((kv.value)::numeric) AS avg_v
			 FROM pulse_survey_responses r,
			      jsonb_each_text(r.answers_json) AS kv
			 WHERE r.tenant_id = ? AND r.survey_id = ?
			   AND kv.value ~ '^-?[0-9]+(\.[0-9]+)?$'
			 GROUP BY kv.key`,
			tenantID, surveyID,
		).Scan(&rows).Error; err != nil {
			return fmt.Errorf("talent: aggregate answers: %w", err)
		}
		for _, r := range rows {
			agg.AnswerSummary[r.Key] = r.Avg
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &agg, nil
}

// ReadFreeTextInput holds parameters for reading a response's free-text answer.
type ReadFreeTextInput struct {
	TenantID   uuid.UUID
	ActorID    uuid.UUID
	ResponseID uuid.UUID
	IP         *string
}

// ReadFreeText decrypts and returns a single response's free-text answer.
//
// Multi-layer permission enforcement:
//   - Layer 1 (HTTP): the route requires survey:read_freetext via
//     RequirePermission middleware.
//   - Layer 2 (Service, here): the service re-validates survey:read_freetext via
//     LoadUserPermissions inside the transaction, so callers that bypass the HTTP
//     layer (batch jobs, internal calls) still cannot obtain plaintext.
//
// The decrypted value is returned ONLY as the function result; it is never
// written to the audit log, notification, or any other log.
func (s *Service) ReadFreeText(ctx context.Context, in ReadFreeTextInput) ([]byte, error) {
	var plaintext []byte
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Defence-in-depth permission re-check.
		perms, err := platformauth.LoadUserPermissions(tx, in.TenantID, in.ActorID)
		if err != nil {
			return fmt.Errorf("talent: read free_text load permissions: %w", err)
		}
		if !platformauth.HasPermission(perms, permFreeText) {
			return ErrForbidden
		}

		var row struct {
			FreeText []byte `gorm:"column:free_text"`
			Found    int    `gorm:"column:found"`
		}
		if err := tx.Raw(
			`SELECT free_text, 1 AS found FROM pulse_survey_responses
			 WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.ResponseID, in.TenantID,
		).Scan(&row).Error; err != nil {
			return fmt.Errorf("talent: read free_text select: %w", err)
		}
		if row.Found == 0 {
			return ErrNotFound
		}

		// Audit the access of free-text (metadata only — never the content).
		idStr := in.ResponseID.String()
		if err := audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "pulse_survey_response.read_freetext",
			ResourceType: "pulse_survey_response",
			ResourceID:   &idStr,
			IP:           in.IP,
		}); err != nil {
			return err
		}

		if len(row.FreeText) > 0 {
			plain, err := crypto.Decrypt(row.FreeText)
			if err != nil {
				return fmt.Errorf("talent: decrypt free_text: %w", err)
			}
			plaintext = plain
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return plaintext, nil
}
