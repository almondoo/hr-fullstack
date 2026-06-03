package goal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/approval"
	"github.com/your-org/hr-saas/internal/platform/audit"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// Sentinel errors.
var (
	ErrNotFound          = errors.New("goal: not found")
	ErrInvalidTransition = errors.New("goal: invalid status transition")
	ErrForbidden         = errors.New("goal: permission denied")
	ErrCycleClosed       = errors.New("goal: cycle is closed (read-only)")
	ErrCascadeCycle      = errors.New("goal: parent link would create a cascade cycle")
	ErrCascadeTooDeep    = errors.New("goal: cascade depth exceeds the configured maximum")
	ErrInvalidInput      = errors.New("goal: invalid input")
)

// allowedGoalTransitions defines the legal goal status moves (finite state
// machine).  Terminal states (achieved, closed) have no outgoing transitions.
//
//	draft       → submitted
//	submitted   → approved (上司承認) | draft (差戻し)
//	approved    → in_progress
//	in_progress → achieved | closed
//
// Submit (draft→submitted) and approve (submitted→approved) and reject
// (submitted→draft) are driven through the approval engine in the same
// transaction so that goals.status and the approval_request never diverge.
var allowedGoalTransitions = map[string]map[string]bool{
	GoalStatusDraft: {
		GoalStatusSubmitted: true,
	},
	GoalStatusSubmitted: {
		GoalStatusApproved: true,
		GoalStatusDraft:    true, // 差戻し (return to requester)
	},
	GoalStatusApproved: {
		GoalStatusInProgress: true,
	},
	GoalStatusInProgress: {
		GoalStatusAchieved: true,
		GoalStatusClosed:   true,
	},
}

// isGoalTransitionAllowed reports whether moving from current → next is valid.
func isGoalTransitionAllowed(current, next string) bool {
	if allowed, ok := allowedGoalTransitions[current]; ok {
		return allowed[next]
	}
	return false
}

// Service provides business logic for the goal-management domain.
//
// The approval engine is constructed internally (not injected) so that the
// RegisterRoutes / NewService signatures stay uniform across all stories while
// still allowing the submit/approve flow to participate in the same DB
// transaction via approval.SubmitTx.
type Service struct {
	tdb         *tenantdb.TenantDB
	approvalSvc *approval.Service
}

// NewService constructs a Service with an internally-built approval engine.
func NewService(tdb *tenantdb.TenantDB) *Service {
	return &Service{tdb: tdb, approvalSvc: approval.NewService(tdb)}
}

// ---------------------------------------------------------------------------
// Review cycles (評価サイクル / 期)
// ---------------------------------------------------------------------------

// CreateCycleInput holds fields for creating a review cycle.
type CreateCycleInput struct {
	TenantID         uuid.UUID
	ActorID          uuid.UUID
	Name             string
	StartsOn         string // YYYY-MM-DD, parsed by caller into time and re-stringified
	EndsOn           string
	GoalDueOn        *string
	ReviewDueOn      *string
	RequireWeight100 bool
	ProgressMethod   string // "average" | "weighted"; defaulted to average when empty
	MaxCascadeDepth  int    // <=0 falls back to 10
	IP               *string
}

// CreateCycle creates a new review cycle in draft status.
func (s *Service) CreateCycle(ctx context.Context, in CreateCycleInput) (*ReviewCycle, error) {
	method := in.ProgressMethod
	if method == "" {
		method = ProgressMethodAverage
	}
	if method != ProgressMethodAverage && method != ProgressMethodWeighted {
		return nil, fmt.Errorf("%w: progress_method must be average or weighted", ErrInvalidInput)
	}
	depth := in.MaxCascadeDepth
	if depth <= 0 {
		depth = 10
	}

	cycle := ReviewCycle{
		ID:               uuid.New(),
		TenantID:         in.TenantID,
		Name:             in.Name,
		Status:           CycleStatusDraft,
		RequireWeight100: in.RequireWeight100,
		ProgressMethod:   method,
		MaxCascadeDepth:  depth,
	}

	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		if err := tx.Exec(
			`INSERT INTO review_cycles
			   (id, tenant_id, name, starts_on, ends_on, goal_due_on, review_due_on,
			    status, require_weight_100, progress_method, max_cascade_depth)
			 VALUES (?, ?, ?, ?::date, ?::date, ?::date, ?::date, ?, ?, ?, ?)`,
			cycle.ID, cycle.TenantID, cycle.Name, in.StartsOn, in.EndsOn,
			nullableDate(in.GoalDueOn), nullableDate(in.ReviewDueOn),
			cycle.Status, cycle.RequireWeight100, cycle.ProgressMethod, cycle.MaxCascadeDepth,
		).Error; err != nil {
			return fmt.Errorf("goal: create cycle insert: %w", err)
		}
		if err := s.reReadCycle(tx, in.TenantID, cycle.ID, &cycle); err != nil {
			return err
		}
		idStr := cycle.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "review_cycle.created",
			ResourceType: "review_cycle",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &cycle, nil
}

// UpdateCycleStatusInput holds fields for a cycle status change.
type UpdateCycleStatusInput struct {
	TenantID uuid.UUID
	ActorID  uuid.UUID
	CycleID  uuid.UUID
	Status   string // draft | active | closed
	IP       *string
}

// allowedCycleTransitions defines legal cycle status moves.
//
//	draft  → active
//	active → closed
//	(closed is terminal)
var allowedCycleTransitions = map[string]map[string]bool{
	CycleStatusDraft:  {CycleStatusActive: true},
	CycleStatusActive: {CycleStatusClosed: true},
}

// UpdateCycleStatus transitions a cycle (draft→active→closed).
func (s *Service) UpdateCycleStatus(ctx context.Context, in UpdateCycleStatusInput) (*ReviewCycle, error) {
	var cycle ReviewCycle
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		var current struct {
			Status string `gorm:"column:status"`
		}
		if err := tx.Raw(
			`SELECT status FROM review_cycles WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.CycleID, in.TenantID,
		).Scan(&current).Error; err != nil {
			return fmt.Errorf("goal: cycle status read: %w", err)
		}
		if current.Status == "" {
			return ErrNotFound
		}
		next, ok := allowedCycleTransitions[current.Status]
		if !ok || !next[in.Status] {
			return fmt.Errorf("%w: cycle %s → %s", ErrInvalidTransition, current.Status, in.Status)
		}
		res := tx.Exec(
			`UPDATE review_cycles SET status = ?, updated_at = now()
			 WHERE id = ? AND tenant_id = ?`,
			in.Status, in.CycleID, in.TenantID,
		)
		if res.Error != nil {
			return fmt.Errorf("goal: cycle status update: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}
		if err := s.reReadCycle(tx, in.TenantID, in.CycleID, &cycle); err != nil {
			return err
		}
		idStr := in.CycleID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "review_cycle.status_updated",
			ResourceType: "review_cycle",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &cycle, nil
}

// GetCycle fetches a review cycle by ID within the tenant.
func (s *Service) GetCycle(ctx context.Context, tenantID, id uuid.UUID) (*ReviewCycle, error) {
	var cycle ReviewCycle
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return s.reReadCycle(tx, tenantID, id, &cycle)
	})
	if err != nil {
		return nil, err
	}
	return &cycle, nil
}

// ListCycles returns all review cycles for a tenant, newest first.
func (s *Service) ListCycles(ctx context.Context, tenantID uuid.UUID) ([]ReviewCycle, error) {
	var cycles []ReviewCycle
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, name, starts_on, ends_on, goal_due_on, review_due_on,
			        status, require_weight_100, progress_method, max_cascade_depth,
			        created_at, updated_at
			 FROM review_cycles
			 WHERE tenant_id = ?
			 ORDER BY starts_on DESC, created_at DESC`,
			tenantID,
		).Scan(&cycles).Error
	})
	if err != nil {
		return nil, err
	}
	return cycles, nil
}

// reReadCycle loads a cycle into dst; returns ErrNotFound when absent.
func (s *Service) reReadCycle(tx *gorm.DB, tenantID, id uuid.UUID, dst *ReviewCycle) error {
	if err := tx.Raw(
		`SELECT id, tenant_id, name, starts_on, ends_on, goal_due_on, review_due_on,
		        status, require_weight_100, progress_method, max_cascade_depth,
		        created_at, updated_at
		 FROM review_cycles
		 WHERE id = ? AND tenant_id = ? LIMIT 1`,
		id, tenantID,
	).Scan(dst).Error; err != nil {
		return fmt.Errorf("goal: read cycle: %w", err)
	}
	if dst.ID == uuid.Nil {
		return ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------------------
// Goals
// ---------------------------------------------------------------------------

// CreateGoalInput holds fields for creating a goal.
type CreateGoalInput struct {
	TenantID        uuid.UUID
	ActorID         uuid.UUID
	ActorEmployeeID *uuid.UUID // resolved by handler; nil means skip ownership check (admin path)
	CycleID         uuid.UUID
	EmployeeID      uuid.UUID
	ParentGoalID    *uuid.UUID
	Method          string // mbo | okr
	Title           string
	Description     string
	Weight          *float64
	SelfRating      *string
	IP              *string
}

// CreateGoal creates a goal in draft status against an active cycle.
//
// Guards:
//   - The cycle must exist, belong to the tenant, and be active (closed cycles
//     are read-only → ErrCycleClosed; draft cycles cannot receive goals yet).
//   - The employee must belong to the tenant (composite FK is a backstop; the
//     explicit COUNT check yields ErrNotFound rather than a raw FK error).
//   - When parent_goal_id is set, the parent must belong to the same tenant
//     and the resulting link must not create a cascade cycle, nor exceed the
//     cycle's max_cascade_depth.
func (s *Service) CreateGoal(ctx context.Context, in CreateGoalInput) (*Goal, error) {
	if in.Method != MethodMBO && in.Method != MethodOKR {
		return nil, fmt.Errorf("%w: method must be mbo or okr", ErrInvalidInput)
	}

	g := Goal{
		ID:           uuid.New(),
		TenantID:     in.TenantID,
		CycleID:      in.CycleID,
		EmployeeID:   in.EmployeeID,
		ParentGoalID: in.ParentGoalID,
		Method:       in.Method,
		Title:        in.Title,
		Description:  in.Description,
		Weight:       in.Weight,
		Status:       GoalStatusDraft,
		SelfRating:   in.SelfRating,
		ProgressPct:  0,
	}

	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Ownership check: actor can only create goals for themselves unless they
		// hold goal:read_all (manager / HR admin path).
		if err := checkGoalOwnership(tx, in.TenantID, in.ActorID, in.ActorEmployeeID, in.EmployeeID); err != nil {
			return err
		}

		// Cycle must be active.
		cycleStatus, err := loadCycleStatus(tx, in.TenantID, in.CycleID)
		if err != nil {
			return err
		}
		if cycleStatus == CycleStatusClosed {
			return ErrCycleClosed
		}
		if cycleStatus != CycleStatusActive {
			return fmt.Errorf("%w: cycle status is %q (goals require an active cycle)", ErrInvalidInput, cycleStatus)
		}

		// Employee must belong to the tenant.
		var empCount int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM employees WHERE id = ? AND tenant_id = ?`,
			in.EmployeeID, in.TenantID,
		).Scan(&empCount).Error; err != nil {
			return fmt.Errorf("goal: create goal verify employee: %w", err)
		}
		if empCount == 0 {
			return ErrNotFound
		}

		// Validate parent link (existence, tenant, cycle, no cascade cycle, depth).
		if in.ParentGoalID != nil {
			if err := s.validateParentLink(tx, in.TenantID, in.CycleID, g.ID, *in.ParentGoalID); err != nil {
				return err
			}
		}

		if err := tx.Exec(
			`INSERT INTO goals
			   (id, tenant_id, cycle_id, employee_id, parent_goal_id, method,
			    title, description, weight, status, self_rating, progress_pct)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			g.ID, g.TenantID, g.CycleID, g.EmployeeID, g.ParentGoalID, g.Method,
			g.Title, g.Description, g.Weight, g.Status, g.SelfRating, g.ProgressPct,
		).Error; err != nil {
			return fmt.Errorf("goal: create goal insert: %w", err)
		}
		if err := s.reReadGoal(tx, in.TenantID, g.ID, &g); err != nil {
			return err
		}
		idStr := g.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "goal.created",
			ResourceType: "goal",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &g, nil
}

// UpdateGoalInput holds editable goal fields (draft/approved/in_progress only).
type UpdateGoalInput struct {
	TenantID        uuid.UUID
	ActorID         uuid.UUID
	ActorEmployeeID *uuid.UUID // resolved by handler; nil means skip ownership check (admin path)
	GoalID          uuid.UUID
	Title           string
	Description     string
	Weight          *float64
	SelfRating      *string
	ParentGoalID    *uuid.UUID
	SetParentGoalID bool // true = write ParentGoalID (even if nil); false = leave existing parent unchanged
	IP              *string
}

// UpdateGoal updates editable fields of a goal.  Goals in a closed cycle are
// read-only.  parent_goal_id changes are re-validated for cascade cycles.
//
// parent_goal_id handling: SetParentGoalID must be true to write the
// parent_goal_id column.  When SetParentGoalID is false the existing link is
// left unchanged, preventing the "title-only PUT wipes the parent" bug.
func (s *Service) UpdateGoal(ctx context.Context, in UpdateGoalInput) (*Goal, error) {
	var g Goal
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		var cur Goal
		if err := s.reReadGoal(tx, in.TenantID, in.GoalID, &cur); err != nil {
			return err
		}

		// Ownership check: actor may only edit their own goals unless privileged.
		if err := checkGoalOwnership(tx, in.TenantID, in.ActorID, in.ActorEmployeeID, cur.EmployeeID); err != nil {
			return err
		}

		// Closed cycle → read-only.
		cycleStatus, err := loadCycleStatus(tx, in.TenantID, cur.CycleID)
		if err != nil {
			return err
		}
		if cycleStatus == CycleStatusClosed {
			return ErrCycleClosed
		}

		// Determine whether to write parent_goal_id:
		//   - SetParentGoalID explicitly true  → write (even if nil = unlink)
		//   - ParentGoalID non-nil             → write (implicit set)
		//   - both false/nil                   → skip (keep existing link)
		writeParent := in.SetParentGoalID || in.ParentGoalID != nil

		if writeParent && in.ParentGoalID != nil {
			if err := s.validateParentLink(tx, in.TenantID, cur.CycleID, in.GoalID, *in.ParentGoalID); err != nil {
				return err
			}
		}

		// Build dynamic SET clause to avoid overwriting parent_goal_id when not
		// requested.
		var res *gorm.DB
		if writeParent {
			res = tx.Exec(
				`UPDATE goals
				 SET title = ?, description = ?, weight = ?, self_rating = ?,
				     parent_goal_id = ?, updated_at = now()
				 WHERE id = ? AND tenant_id = ?`,
				in.Title, in.Description, in.Weight, in.SelfRating, in.ParentGoalID,
				in.GoalID, in.TenantID,
			)
		} else {
			res = tx.Exec(
				`UPDATE goals
				 SET title = ?, description = ?, weight = ?, self_rating = ?,
				     updated_at = now()
				 WHERE id = ? AND tenant_id = ?`,
				in.Title, in.Description, in.Weight, in.SelfRating,
				in.GoalID, in.TenantID,
			)
		}
		if res.Error != nil {
			return fmt.Errorf("goal: update goal: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}
		if err := s.reReadGoal(tx, in.TenantID, in.GoalID, &g); err != nil {
			return err
		}
		idStr := in.GoalID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "goal.updated",
			ResourceType: "goal",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &g, nil
}

// GetGoal fetches a goal by ID within the tenant.
func (s *Service) GetGoal(ctx context.Context, tenantID, id uuid.UUID) (*Goal, error) {
	var g Goal
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return s.reReadGoal(tx, tenantID, id, &g)
	})
	if err != nil {
		return nil, err
	}
	return &g, nil
}

// ListGoals returns goals for a cycle, optionally filtered by employee.
func (s *Service) ListGoals(ctx context.Context, tenantID, cycleID uuid.UUID, employeeID *uuid.UUID) ([]Goal, error) {
	var goals []Goal
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		q := `SELECT id, tenant_id, cycle_id, employee_id, parent_goal_id, method,
		             title, description, weight, status, self_rating, progress_pct,
		             approval_request_id, created_at, updated_at
		      FROM goals
		      WHERE tenant_id = ? AND cycle_id = ?`
		args := []any{tenantID, cycleID}
		if employeeID != nil {
			q += ` AND employee_id = ?`
			args = append(args, *employeeID)
		}
		q += ` ORDER BY created_at`
		return tx.Raw(q, args...).Scan(&goals).Error
	})
	if err != nil {
		return nil, err
	}
	return goals, nil
}

// reReadGoal loads a goal into dst; returns ErrNotFound when absent.
func (s *Service) reReadGoal(tx *gorm.DB, tenantID, id uuid.UUID, dst *Goal) error {
	if err := tx.Raw(
		`SELECT id, tenant_id, cycle_id, employee_id, parent_goal_id, method,
		        title, description, weight, status, self_rating, progress_pct,
		        approval_request_id, created_at, updated_at
		 FROM goals
		 WHERE id = ? AND tenant_id = ? LIMIT 1`,
		id, tenantID,
	).Scan(dst).Error; err != nil {
		return fmt.Errorf("goal: read goal: %w", err)
	}
	if dst.ID == uuid.Nil {
		return ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------------------
// Cascade — parent links, cycle detection, tree retrieval
// ---------------------------------------------------------------------------

// validateParentLink verifies that linking goalID → parentID is permissible:
//   - parent exists in the same tenant (composite FK is a backstop; explicit
//     check yields ErrNotFound),
//   - parent belongs to the same cycle,
//   - the link does not form a cycle (parent is not goalID itself and goalID
//     is not an ancestor of parent),
//   - the resulting chain depth does not exceed the cycle's max_cascade_depth.
//
// goalID may be uuid.Nil at creation time (the row does not yet exist); the
// self-cycle case is still caught because parentID is walked upward and goalID
// is compared at each hop.
func (s *Service) validateParentLink(tx *gorm.DB, tenantID, cycleID, goalID, parentID uuid.UUID) error {
	if parentID == goalID {
		return ErrCascadeCycle
	}

	// Parent must exist in this tenant and the same cycle.
	var parent struct {
		CycleID uuid.UUID `gorm:"column:cycle_id"`
	}
	if err := tx.Raw(
		`SELECT cycle_id FROM goals WHERE id = ? AND tenant_id = ? LIMIT 1`,
		parentID, tenantID,
	).Scan(&parent).Error; err != nil {
		return fmt.Errorf("goal: validate parent read: %w", err)
	}
	if parent.CycleID == uuid.Nil {
		return ErrNotFound
	}
	if parent.CycleID != cycleID {
		return fmt.Errorf("%w: parent goal belongs to a different cycle", ErrInvalidInput)
	}

	maxDepth, err := loadCycleMaxDepth(tx, tenantID, cycleID)
	if err != nil {
		return err
	}

	// Walk ancestors of parentID upward.  If we reach goalID, linking would
	// create a cycle.  Track depth to enforce max_cascade_depth and to provide
	// a hard stop against any unexpected loop in stored data.
	cursor := parentID
	depth := 1 // the edge goalID → parentID counts as depth 1
	for {
		if cursor == goalID {
			return ErrCascadeCycle
		}
		var row struct {
			Parent *uuid.UUID `gorm:"column:parent_goal_id"`
		}
		if err := tx.Raw(
			`SELECT parent_goal_id FROM goals WHERE id = ? AND tenant_id = ? LIMIT 1`,
			cursor, tenantID,
		).Scan(&row).Error; err != nil {
			return fmt.Errorf("goal: validate parent walk: %w", err)
		}
		if row.Parent == nil {
			break
		}
		depth++
		if depth > maxDepth {
			return ErrCascadeTooDeep
		}
		cursor = *row.Parent
	}
	return nil
}

// CascadeNode is one node of a cascade tree (a goal plus its children).
type CascadeNode struct {
	Goal     Goal
	Children []*CascadeNode
}

// GetCascadeTree returns the subtree rooted at rootGoalID, with children
// resolved recursively within the tenant and cycle.
func (s *Service) GetCascadeTree(ctx context.Context, tenantID, rootGoalID uuid.UUID) (*CascadeNode, error) {
	var root *CascadeNode
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		var rootGoal Goal
		if err := s.reReadGoal(tx, tenantID, rootGoalID, &rootGoal); err != nil {
			return err
		}
		// Load all goals in the cycle once, then build the tree in memory to
		// avoid N+1 queries and any risk of unbounded recursion in SQL.
		var all []Goal
		if err := tx.Raw(
			`SELECT id, tenant_id, cycle_id, employee_id, parent_goal_id, method,
			        title, description, weight, status, self_rating, progress_pct,
			        approval_request_id, created_at, updated_at
			 FROM goals
			 WHERE tenant_id = ? AND cycle_id = ?`,
			tenantID, rootGoal.CycleID,
		).Scan(&all).Error; err != nil {
			return fmt.Errorf("goal: cascade tree load: %w", err)
		}
		root = buildCascadeTree(rootGoal, all)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return root, nil
}

// buildCascadeTree assembles the subtree rooted at root from the flat slice
// all.  A visited set bounds the traversal so malformed parent links cannot
// cause infinite recursion.
func buildCascadeTree(root Goal, all []Goal) *CascadeNode {
	childrenByParent := make(map[uuid.UUID][]Goal)
	for _, g := range all {
		if g.ParentGoalID != nil {
			childrenByParent[*g.ParentGoalID] = append(childrenByParent[*g.ParentGoalID], g)
		}
	}
	visited := make(map[uuid.UUID]bool)
	var build func(g Goal) *CascadeNode
	build = func(g Goal) *CascadeNode {
		node := &CascadeNode{Goal: g}
		if visited[g.ID] {
			return node
		}
		visited[g.ID] = true
		for _, child := range childrenByParent[g.ID] {
			node.Children = append(node.Children, build(child))
		}
		return node
	}
	return build(root)
}

// ---------------------------------------------------------------------------
// MBO weight integrity
// ---------------------------------------------------------------------------

// MBOWeightTotal returns the sum of weights of MBO goals for the given
// (employee, cycle).  NULL weights count as 0.  This is a helper for the
// optional submit-time 100% check; over-100% is a warning, not a hard error,
// during editing.
func (s *Service) MBOWeightTotal(ctx context.Context, tenantID, cycleID, employeeID uuid.UUID) (float64, error) {
	var total float64
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT COALESCE(SUM(weight), 0) FROM goals
			 WHERE tenant_id = ? AND cycle_id = ? AND employee_id = ? AND method = ?`,
			tenantID, cycleID, employeeID, MethodMBO,
		).Scan(&total).Error
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}

// mboWeightTotalTx is the in-transaction variant used during the submit guard.
func mboWeightTotalTx(tx *gorm.DB, tenantID, cycleID, employeeID uuid.UUID) (float64, error) {
	var total float64
	if err := tx.Raw(
		`SELECT COALESCE(SUM(weight), 0) FROM goals
		 WHERE tenant_id = ? AND cycle_id = ? AND employee_id = ? AND method = ?`,
		tenantID, cycleID, employeeID, MethodMBO,
	).Scan(&total).Error; err != nil {
		return 0, fmt.Errorf("goal: mbo weight total: %w", err)
	}
	return total, nil
}

// ---------------------------------------------------------------------------
// Key results (OKR)
// ---------------------------------------------------------------------------

// AddKeyResultInput holds fields for adding a KeyResult to an OKR goal.
type AddKeyResultInput struct {
	TenantID        uuid.UUID
	ActorID         uuid.UUID
	ActorEmployeeID *uuid.UUID // resolved by handler; nil means skip ownership check
	GoalID          uuid.UUID
	Title           string
	MetricUnit      string
	StartValue      float64
	TargetValue     float64
	CurrentValue    float64
	IP              *string
}

// AddKeyResult adds a KeyResult to an OKR goal and recomputes the Objective
// progress.  Only goals with method=okr may have KeyResults.
func (s *Service) AddKeyResult(ctx context.Context, in AddKeyResultInput) (*KeyResult, error) {
	kr := KeyResult{
		ID:           uuid.New(),
		TenantID:     in.TenantID,
		GoalID:       in.GoalID,
		Title:        in.Title,
		MetricUnit:   in.MetricUnit,
		StartValue:   in.StartValue,
		TargetValue:  in.TargetValue,
		CurrentValue: in.CurrentValue,
		ProgressPct:  computeKRProgress(in.StartValue, in.TargetValue, in.CurrentValue),
	}

	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		var g Goal
		if err := s.reReadGoal(tx, in.TenantID, in.GoalID, &g); err != nil {
			return err
		}

		// Ownership check: actor may only add key results to their own OKR goals.
		if err := checkGoalOwnership(tx, in.TenantID, in.ActorID, in.ActorEmployeeID, g.EmployeeID); err != nil {
			return err
		}

		if g.Method != MethodOKR {
			return fmt.Errorf("%w: key results are only valid for OKR goals", ErrInvalidInput)
		}
		cycleStatus, err := loadCycleStatus(tx, in.TenantID, g.CycleID)
		if err != nil {
			return err
		}
		if cycleStatus == CycleStatusClosed {
			return ErrCycleClosed
		}

		if err := tx.Exec(
			`INSERT INTO key_results
			   (id, tenant_id, goal_id, title, metric_unit, start_value,
			    target_value, current_value, progress_pct)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			kr.ID, kr.TenantID, kr.GoalID, kr.Title, kr.MetricUnit,
			kr.StartValue, kr.TargetValue, kr.CurrentValue, kr.ProgressPct,
		).Error; err != nil {
			return fmt.Errorf("goal: add key result insert: %w", err)
		}

		if err := recomputeObjectiveProgress(tx, in.TenantID, in.GoalID); err != nil {
			return err
		}

		idStr := kr.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "key_result.created",
			ResourceType: "key_result",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &kr, nil
}

// UpdateKeyResultProgressInput holds fields for updating a KeyResult's current
// value (and therefore its progress).
type UpdateKeyResultProgressInput struct {
	TenantID        uuid.UUID
	ActorID         uuid.UUID
	ActorEmployeeID *uuid.UUID // resolved by handler; nil means skip ownership check
	KeyResultID     uuid.UUID
	CurrentValue    float64
	Comment         string
	IP              *string
}

// UpdateKeyResultProgress sets a KeyResult's current value, recomputes its
// progress and the parent Objective progress, and appends a progress log.
func (s *Service) UpdateKeyResultProgress(ctx context.Context, in UpdateKeyResultProgressInput) (*KeyResult, error) {
	var kr KeyResult
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Load KR (and lock the row to avoid lost updates on concurrent writes).
		if err := tx.Raw(
			`SELECT id, tenant_id, goal_id, title, metric_unit, start_value,
			        target_value, current_value, progress_pct, created_at, updated_at
			 FROM key_results
			 WHERE id = ? AND tenant_id = ? LIMIT 1
			 FOR UPDATE`,
			in.KeyResultID, in.TenantID,
		).Scan(&kr).Error; err != nil {
			return fmt.Errorf("goal: update kr progress read: %w", err)
		}
		if kr.ID == uuid.Nil {
			return ErrNotFound
		}

		// Ownership check: actor may only update KR progress for their own goals.
		var goalOwner struct {
			EmployeeID uuid.UUID `gorm:"column:employee_id"`
		}
		if err := tx.Raw(
			`SELECT employee_id FROM goals WHERE id = ? AND tenant_id = ? LIMIT 1`,
			kr.GoalID, in.TenantID,
		).Scan(&goalOwner).Error; err != nil {
			return fmt.Errorf("goal: update kr progress load goal owner: %w", err)
		}
		if err := checkGoalOwnership(tx, in.TenantID, in.ActorID, in.ActorEmployeeID, goalOwner.EmployeeID); err != nil {
			return err
		}

		cycleStatus, err := loadGoalCycleStatus(tx, in.TenantID, kr.GoalID)
		if err != nil {
			return err
		}
		if cycleStatus == CycleStatusClosed {
			return ErrCycleClosed
		}

		newPct := computeKRProgress(kr.StartValue, kr.TargetValue, in.CurrentValue)
		if err := tx.Exec(
			`UPDATE key_results
			 SET current_value = ?, progress_pct = ?, updated_at = now()
			 WHERE id = ? AND tenant_id = ?`,
			in.CurrentValue, newPct, in.KeyResultID, in.TenantID,
		).Error; err != nil {
			return fmt.Errorf("goal: update kr progress: %w", err)
		}

		if err := recomputeObjectiveProgress(tx, in.TenantID, kr.GoalID); err != nil {
			return err
		}

		// Append a progress log referencing the KeyResult.
		krID := in.KeyResultID
		logID := uuid.New()
		if err := tx.Exec(
			`INSERT INTO goal_progress_logs
			   (id, tenant_id, goal_id, key_result_id, progress_pct, comment, updated_by_user_id)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			logID, in.TenantID, kr.GoalID, krID, newPct, in.Comment, in.ActorID,
		).Error; err != nil {
			return fmt.Errorf("goal: update kr progress log: %w", err)
		}

		if err := tx.Raw(
			`SELECT id, tenant_id, goal_id, title, metric_unit, start_value,
			        target_value, current_value, progress_pct, created_at, updated_at
			 FROM key_results WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.KeyResultID, in.TenantID,
		).Scan(&kr).Error; err != nil {
			return fmt.Errorf("goal: update kr progress re-read: %w", err)
		}

		idStr := in.KeyResultID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "key_result.progress_updated",
			ResourceType: "key_result",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &kr, nil
}

// ListKeyResults returns the KeyResults of an OKR goal.
func (s *Service) ListKeyResults(ctx context.Context, tenantID, goalID uuid.UUID) ([]KeyResult, error) {
	var krs []KeyResult
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, goal_id, title, metric_unit, start_value,
			        target_value, current_value, progress_pct, created_at, updated_at
			 FROM key_results
			 WHERE tenant_id = ? AND goal_id = ?
			 ORDER BY created_at`,
			tenantID, goalID,
		).Scan(&krs).Error
	})
	if err != nil {
		return nil, err
	}
	return krs, nil
}

// computeKRProgress implements:
//
//	progress = clamp((current - start) / (target - start), 0, 1) * 100
//
// Degenerate case target == start: 0% unless current has reached/exceeded the
// target, in which case 100% — this avoids division by zero.
func computeKRProgress(start, target, current float64) float64 {
	denom := target - start
	if denom == 0 {
		if current >= target {
			return 100
		}
		return 0
	}
	ratio := (current - start) / denom
	if ratio < 0 {
		ratio = 0
	}
	if ratio > 1 {
		ratio = 1
	}
	return ratio * 100
}

// recomputeObjectiveProgress sets goals.progress_pct to the average of its
// KeyResults' progress (the spec's default; weighted is a future extension via
// the cycle's progress_method setting).  Goals with no KeyResults keep 0.
func recomputeObjectiveProgress(tx *gorm.DB, tenantID, goalID uuid.UUID) error {
	var avg struct {
		Avg *float64 `gorm:"column:avg"`
	}
	if err := tx.Raw(
		`SELECT AVG(progress_pct) AS avg FROM key_results
		 WHERE tenant_id = ? AND goal_id = ?`,
		tenantID, goalID,
	).Scan(&avg).Error; err != nil {
		return fmt.Errorf("goal: recompute objective progress: %w", err)
	}
	var pct float64
	if avg.Avg != nil {
		pct = *avg.Avg
	}
	if err := tx.Exec(
		`UPDATE goals SET progress_pct = ?, updated_at = now()
		 WHERE id = ? AND tenant_id = ?`,
		pct, goalID, tenantID,
	).Error; err != nil {
		return fmt.Errorf("goal: recompute objective progress update: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// MBO / whole-goal progress logging
// ---------------------------------------------------------------------------

// UpdateGoalProgressInput holds fields for an MBO/whole-goal progress update.
type UpdateGoalProgressInput struct {
	TenantID        uuid.UUID
	ActorID         uuid.UUID
	ActorEmployeeID *uuid.UUID // resolved by handler; nil means skip ownership check
	GoalID          uuid.UUID
	ProgressPct     float64
	Comment         string
	IP              *string
}

// UpdateGoalProgress sets the whole-goal progress (used for MBO goals where
// progress is reported directly rather than derived from KeyResults) and
// appends a progress log with key_result_id = NULL.
func (s *Service) UpdateGoalProgress(ctx context.Context, in UpdateGoalProgressInput) (*Goal, error) {
	if in.ProgressPct < 0 || in.ProgressPct > 100 {
		return nil, fmt.Errorf("%w: progress_pct must be between 0 and 100", ErrInvalidInput)
	}
	var g Goal
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		var cur Goal
		if err := s.reReadGoal(tx, in.TenantID, in.GoalID, &cur); err != nil {
			return err
		}

		// Ownership check: actor may only update progress of their own goals.
		if err := checkGoalOwnership(tx, in.TenantID, in.ActorID, in.ActorEmployeeID, cur.EmployeeID); err != nil {
			return err
		}

		cycleStatus, err := loadCycleStatus(tx, in.TenantID, cur.CycleID)
		if err != nil {
			return err
		}
		if cycleStatus == CycleStatusClosed {
			return ErrCycleClosed
		}

		if err := tx.Exec(
			`UPDATE goals SET progress_pct = ?, updated_at = now()
			 WHERE id = ? AND tenant_id = ?`,
			in.ProgressPct, in.GoalID, in.TenantID,
		).Error; err != nil {
			return fmt.Errorf("goal: update goal progress: %w", err)
		}

		logID := uuid.New()
		if err := tx.Exec(
			`INSERT INTO goal_progress_logs
			   (id, tenant_id, goal_id, key_result_id, progress_pct, comment, updated_by_user_id)
			 VALUES (?, ?, ?, NULL, ?, ?, ?)`,
			logID, in.TenantID, in.GoalID, in.ProgressPct, in.Comment, in.ActorID,
		).Error; err != nil {
			return fmt.Errorf("goal: update goal progress log: %w", err)
		}

		if err := s.reReadGoal(tx, in.TenantID, in.GoalID, &g); err != nil {
			return err
		}
		idStr := in.GoalID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "goal.progress_updated",
			ResourceType: "goal",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &g, nil
}

// ListProgressLogs returns the progress history of a goal, newest first.
func (s *Service) ListProgressLogs(ctx context.Context, tenantID, goalID uuid.UUID) ([]ProgressLog, error) {
	var logs []ProgressLog
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, goal_id, key_result_id, progress_pct, comment,
			        updated_by_user_id, created_at
			 FROM goal_progress_logs
			 WHERE tenant_id = ? AND goal_id = ?
			 ORDER BY created_at DESC`,
			tenantID, goalID,
		).Scan(&logs).Error
	})
	if err != nil {
		return nil, err
	}
	return logs, nil
}

// ---------------------------------------------------------------------------
// Status transitions + approval-engine integration
// ---------------------------------------------------------------------------

// SubmitGoalInput holds fields for submitting a goal for approval.
type SubmitGoalInput struct {
	TenantID        uuid.UUID
	ActorID         uuid.UUID
	ActorEmployeeID *uuid.UUID // resolved by handler; nil means skip ownership check
	GoalID          uuid.UUID
	DepartmentID    *uuid.UUID // optional, used for approval route resolution
	IP              *string
}

// SubmitGoal transitions a goal draft → submitted and submits it to the
// approval engine in the same transaction.
//
// When the cycle requires weight 100% (require_weight_100) and the goal is an
// MBO goal, the employee's total MBO weight for the cycle must equal 100 before
// submission is allowed (ErrInvalidInput otherwise).
//
// If no approval route is configured (ErrRouteNotFound / ErrRouteEmpty) the
// goal still moves to submitted without a linked approval request — this is the
// intended fallback for tenants that manage approvals manually.  Any other
// approval error rolls back the whole transaction so goals.status and the
// approval_request never diverge.
func (s *Service) SubmitGoal(ctx context.Context, in SubmitGoalInput) (*Goal, error) {
	var g Goal
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		var cur Goal
		if err := s.reReadGoal(tx, in.TenantID, in.GoalID, &cur); err != nil {
			return err
		}

		// Ownership check: only the goal owner (or privileged actor) may submit.
		if err := checkGoalOwnership(tx, in.TenantID, in.ActorID, in.ActorEmployeeID, cur.EmployeeID); err != nil {
			return err
		}

		if !isGoalTransitionAllowed(cur.Status, GoalStatusSubmitted) {
			return fmt.Errorf("%w: %s → %s", ErrInvalidTransition, cur.Status, GoalStatusSubmitted)
		}

		// Optional weight-100% gate at submit time (tenant-configurable).
		requireWeight100, err := loadCycleRequireWeight100(tx, in.TenantID, cur.CycleID)
		if err != nil {
			return err
		}
		if requireWeight100 && cur.Method == MethodMBO {
			total, err := mboWeightTotalTx(tx, in.TenantID, cur.CycleID, cur.EmployeeID)
			if err != nil {
				return err
			}
			if total != 100 {
				return fmt.Errorf("%w: MBO weight total is %.2f, must be 100 to submit", ErrInvalidInput, total)
			}
		}

		if err := tx.Exec(
			`UPDATE goals SET status = ?, updated_at = now()
			 WHERE id = ? AND tenant_id = ?`,
			GoalStatusSubmitted, in.GoalID, in.TenantID,
		).Error; err != nil {
			return fmt.Errorf("goal: submit goal update: %w", err)
		}

		// Submit to the approval engine within the same transaction.
		approvalReq, submitErr := s.approvalSvc.SubmitTx(tx, approval.SubmitInput{
			TenantID:     in.TenantID,
			ActorID:      in.ActorID,
			RequestType:  "goal_approval",
			SubjectRef:   in.GoalID.String(),
			DepartmentID: in.DepartmentID,
			PayloadJSON:  []byte(`{"goal_id":"` + in.GoalID.String() + `"}`),
			IP:           in.IP,
		})
		if submitErr != nil {
			if errors.Is(submitErr, approval.ErrRouteNotFound) || errors.Is(submitErr, approval.ErrRouteEmpty) {
				// No route — goal stays submitted without a link (manual approval).
				if err := s.reReadGoal(tx, in.TenantID, in.GoalID, &g); err != nil {
					return err
				}
				idStr := in.GoalID.String()
				return audit.Record(tx, audit.Entry{
					TenantID:     in.TenantID,
					UserID:       &in.ActorID,
					Action:       "goal.submitted",
					ResourceType: "goal",
					ResourceID:   &idStr,
					IP:           in.IP,
				})
			}
			return fmt.Errorf("goal: submit goal approval: %w", submitErr)
		}

		// Link the approval request to the goal.
		if err := tx.Exec(
			`UPDATE goals SET approval_request_id = ?, updated_at = now()
			 WHERE id = ? AND tenant_id = ?`,
			approvalReq.ID, in.GoalID, in.TenantID,
		).Error; err != nil {
			return fmt.Errorf("goal: submit goal link approval: %w", err)
		}

		if err := s.reReadGoal(tx, in.TenantID, in.GoalID, &g); err != nil {
			return err
		}
		idStr := in.GoalID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "goal.submitted",
			ResourceType: "goal",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &g, nil
}

// TransitionGoalInput holds fields for a direct goal status transition that is
// not the approval-engine submit (approve / return / start / achieve / close).
type TransitionGoalInput struct {
	TenantID uuid.UUID
	ActorID  uuid.UUID
	GoalID   uuid.UUID
	Status   string
	IP       *string
}

// TransitionGoal applies an allow-listed goal status transition.
//
// Allowed via this method:
//
//	submitted   → approved      (上司承認)
//	submitted   → draft         (差戻し)
//	approved    → in_progress
//	in_progress → achieved | closed
//
// draft → submitted is NOT permitted here; use SubmitGoal so the approval
// engine is always engaged.
func (s *Service) TransitionGoal(ctx context.Context, in TransitionGoalInput) (*Goal, error) {
	if in.Status == GoalStatusSubmitted {
		return nil, fmt.Errorf("%w: use SubmitGoal to move a goal to submitted", ErrInvalidTransition)
	}
	var g Goal
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		var cur Goal
		if err := s.reReadGoal(tx, in.TenantID, in.GoalID, &cur); err != nil {
			return err
		}
		if !isGoalTransitionAllowed(cur.Status, in.Status) {
			return fmt.Errorf("%w: %s → %s", ErrInvalidTransition, cur.Status, in.Status)
		}
		if err := tx.Exec(
			`UPDATE goals SET status = ?, updated_at = now()
			 WHERE id = ? AND tenant_id = ?`,
			in.Status, in.GoalID, in.TenantID,
		).Error; err != nil {
			return fmt.Errorf("goal: transition goal update: %w", err)
		}
		if err := s.reReadGoal(tx, in.TenantID, in.GoalID, &g); err != nil {
			return err
		}
		idStr := in.GoalID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "goal.status_" + in.Status,
			ResourceType: "goal",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &g, nil
}

// ---------------------------------------------------------------------------
// Cross-cycle copy (期跨ぎコピー)
// ---------------------------------------------------------------------------

// CopyGoalsInput holds fields for copying goals between cycles.
type CopyGoalsInput struct {
	TenantID    uuid.UUID
	ActorID     uuid.UUID
	FromCycleID uuid.UUID
	ToCycleID   uuid.UUID
	EmployeeID  uuid.UUID
	IP          *string
}

// CopyGoals duplicates an employee's goals (and OKR KeyResults) from a closed
// cycle into a new active cycle.  Progress and self_rating are reset; copied
// goals start in draft.  Parent links are remapped to the copied goals when the
// parent was also copied; otherwise the parent link is dropped (held), avoiding
// any cross-cycle parent reference.
func (s *Service) CopyGoals(ctx context.Context, in CopyGoalsInput) ([]Goal, error) {
	if in.FromCycleID == in.ToCycleID {
		return nil, fmt.Errorf("%w: source and destination cycles must differ", ErrInvalidInput)
	}
	var copied []Goal
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Destination cycle must be active.
		toStatus, err := loadCycleStatus(tx, in.TenantID, in.ToCycleID)
		if err != nil {
			return err
		}
		if toStatus != CycleStatusActive {
			return fmt.Errorf("%w: destination cycle must be active to copy into", ErrInvalidInput)
		}
		// Source cycle must exist (existence check; any status allowed as source,
		// though the typical case is copying out of a closed cycle).
		if _, err := loadCycleStatus(tx, in.TenantID, in.FromCycleID); err != nil {
			return err
		}

		var src []Goal
		if err := tx.Raw(
			`SELECT id, tenant_id, cycle_id, employee_id, parent_goal_id, method,
			        title, description, weight, status, self_rating, progress_pct,
			        approval_request_id, created_at, updated_at
			 FROM goals
			 WHERE tenant_id = ? AND cycle_id = ? AND employee_id = ?
			 ORDER BY created_at`,
			in.TenantID, in.FromCycleID, in.EmployeeID,
		).Scan(&src).Error; err != nil {
			return fmt.Errorf("goal: copy goals load source: %w", err)
		}

		// First pass: insert copies with parent left NULL, recording the old→new
		// ID mapping.
		idMap := make(map[uuid.UUID]uuid.UUID, len(src))
		for i := range src {
			old := src[i]
			newID := uuid.New()
			idMap[old.ID] = newID
			if err := tx.Exec(
				`INSERT INTO goals
				   (id, tenant_id, cycle_id, employee_id, parent_goal_id, method,
				    title, description, weight, status, self_rating, progress_pct)
				 VALUES (?, ?, ?, ?, NULL, ?, ?, ?, ?, ?, NULL, 0)`,
				newID, in.TenantID, in.ToCycleID, old.EmployeeID, old.Method,
				old.Title, old.Description, old.Weight, GoalStatusDraft,
			).Error; err != nil {
				return fmt.Errorf("goal: copy goals insert: %w", err)
			}

			// Copy OKR KeyResults (progress reset).
			if old.Method == MethodOKR {
				var krs []KeyResult
				if err := tx.Raw(
					`SELECT id, tenant_id, goal_id, title, metric_unit, start_value,
					        target_value, current_value, progress_pct, created_at, updated_at
					 FROM key_results WHERE tenant_id = ? AND goal_id = ?`,
					in.TenantID, old.ID,
				).Scan(&krs).Error; err != nil {
					return fmt.Errorf("goal: copy goals load key results: %w", err)
				}
				for j := range krs {
					kr := krs[j]
					if err := tx.Exec(
						`INSERT INTO key_results
						   (id, tenant_id, goal_id, title, metric_unit, start_value,
						    target_value, current_value, progress_pct)
						 VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0)`,
						uuid.New(), in.TenantID, newID, kr.Title, kr.MetricUnit,
						kr.StartValue, kr.TargetValue, kr.StartValue,
					).Error; err != nil {
						return fmt.Errorf("goal: copy goals insert key result: %w", err)
					}
				}
			}
		}

		// Second pass: remap parent links that point to copied goals.
		for i := range src {
			old := src[i]
			if old.ParentGoalID == nil {
				continue
			}
			newParent, ok := idMap[*old.ParentGoalID]
			if !ok {
				continue // parent not copied → leave parent NULL (held)
			}
			newID := idMap[old.ID]
			if err := tx.Exec(
				`UPDATE goals SET parent_goal_id = ?, updated_at = now()
				 WHERE id = ? AND tenant_id = ?`,
				newParent, newID, in.TenantID,
			).Error; err != nil {
				return fmt.Errorf("goal: copy goals remap parent: %w", err)
			}
		}

		// Load the copied goals for the response.
		if err := tx.Raw(
			`SELECT id, tenant_id, cycle_id, employee_id, parent_goal_id, method,
			        title, description, weight, status, self_rating, progress_pct,
			        approval_request_id, created_at, updated_at
			 FROM goals
			 WHERE tenant_id = ? AND cycle_id = ? AND employee_id = ?
			 ORDER BY created_at`,
			in.TenantID, in.ToCycleID, in.EmployeeID,
		).Scan(&copied).Error; err != nil {
			return fmt.Errorf("goal: copy goals reload: %w", err)
		}

		idStr := in.ToCycleID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "goal.cross_cycle_copied",
			ResourceType: "review_cycle",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return copied, nil
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// nullableDate returns the dereferenced string or nil for a nil/empty pointer,
// so that an empty optional date binds as SQL NULL.
func nullableDate(s *string) any {
	if s == nil || *s == "" {
		return nil
	}
	return *s
}

// loadCycleStatus returns the status of a cycle; ErrNotFound when absent.
func loadCycleStatus(tx *gorm.DB, tenantID, cycleID uuid.UUID) (string, error) {
	var row struct {
		Status string `gorm:"column:status"`
	}
	if err := tx.Raw(
		`SELECT status FROM review_cycles WHERE id = ? AND tenant_id = ? LIMIT 1`,
		cycleID, tenantID,
	).Scan(&row).Error; err != nil {
		return "", fmt.Errorf("goal: load cycle status: %w", err)
	}
	if row.Status == "" {
		return "", ErrNotFound
	}
	return row.Status, nil
}

// loadGoalCycleStatus returns the status of the cycle that owns the given goal.
func loadGoalCycleStatus(tx *gorm.DB, tenantID, goalID uuid.UUID) (string, error) {
	var row struct {
		Status string `gorm:"column:status"`
	}
	if err := tx.Raw(
		`SELECT rc.status
		 FROM goals g
		 JOIN review_cycles rc ON rc.id = g.cycle_id AND rc.tenant_id = g.tenant_id
		 WHERE g.id = ? AND g.tenant_id = ? LIMIT 1`,
		goalID, tenantID,
	).Scan(&row).Error; err != nil {
		return "", fmt.Errorf("goal: load goal cycle status: %w", err)
	}
	if row.Status == "" {
		return "", ErrNotFound
	}
	return row.Status, nil
}

// loadCycleMaxDepth returns the cycle's max_cascade_depth setting.
func loadCycleMaxDepth(tx *gorm.DB, tenantID, cycleID uuid.UUID) (int, error) {
	var row struct {
		MaxCascadeDepth int `gorm:"column:max_cascade_depth"`
	}
	if err := tx.Raw(
		`SELECT max_cascade_depth FROM review_cycles WHERE id = ? AND tenant_id = ? LIMIT 1`,
		cycleID, tenantID,
	).Scan(&row).Error; err != nil {
		return 0, fmt.Errorf("goal: load cycle max depth: %w", err)
	}
	if row.MaxCascadeDepth == 0 {
		// Either the cycle does not exist or the value is unset; treat absence as
		// not-found is handled elsewhere — use a safe default here.
		return 10, nil
	}
	return row.MaxCascadeDepth, nil
}

// loadCycleRequireWeight100 returns the cycle's require_weight_100 setting.
func loadCycleRequireWeight100(tx *gorm.DB, tenantID, cycleID uuid.UUID) (bool, error) {
	var row struct {
		RequireWeight100 bool `gorm:"column:require_weight_100"`
	}
	if err := tx.Raw(
		`SELECT require_weight_100 FROM review_cycles WHERE id = ? AND tenant_id = ? LIMIT 1`,
		cycleID, tenantID,
	).Scan(&row).Error; err != nil {
		return false, fmt.Errorf("goal: load cycle require_weight_100: %w", err)
	}
	return row.RequireWeight100, nil
}

// ResolveActorEmployee is the public handler-facing wrapper around
// resolveActorEmployeeID.  It opens its own WithinTenant transaction so the
// handler can call it before constructing the Input struct.  Returns nil (not
// an error) when no employee row is linked to the user — callers treat nil as
// "bypass ownership check" (unlinked admin).
func (s *Service) ResolveActorEmployee(ctx context.Context, tenantID, userID uuid.UUID) *uuid.UUID {
	var empID *uuid.UUID
	_ = s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		id, err := resolveActorEmployeeID(tx, tenantID, userID)
		if err != nil {
			return err
		}
		empID = id
		return nil
	})
	return empID
}

// resolveActorEmployeeID looks up the employee_id linked to the given userID
// within the tenant.  The relationship is stored on the users table
// (users.employee_id → employees.id), not on employees.  Returns (nil, nil)
// when the user has no linked employee record (e.g. an admin-only user).
func resolveActorEmployeeID(tx *gorm.DB, tenantID, userID uuid.UUID) (*uuid.UUID, error) {
	var row struct {
		EmployeeID *uuid.UUID `gorm:"column:employee_id"`
	}
	if err := tx.Raw(
		`SELECT employee_id FROM users WHERE id = ? AND tenant_id = ? LIMIT 1`,
		userID, tenantID,
	).Scan(&row).Error; err != nil {
		return nil, fmt.Errorf("goal: resolve actor employee: %w", err)
	}
	return row.EmployeeID, nil
}

// actorHasPrivilegedGoalAccess reports whether the actor holds the goal:read_all
// (or wildcard) permission, which allows acting on any employee's goal without
// ownership restriction.  Must be called inside a WithinTenant transaction.
func actorHasPrivilegedGoalAccess(tx *gorm.DB, tenantID, userID uuid.UUID) (bool, error) {
	var roleIDRow struct {
		RoleID *uuid.UUID `gorm:"column:role_id"`
	}
	if err := tx.Raw(
		`SELECT role_id FROM users WHERE id = ? AND tenant_id = ? LIMIT 1`,
		userID, tenantID,
	).Scan(&roleIDRow).Error; err != nil {
		return false, fmt.Errorf("goal: load user role_id for perms: %w", err)
	}
	if roleIDRow.RoleID == nil {
		return false, nil
	}
	var permRow struct {
		Permissions []byte `gorm:"column:permissions;type:jsonb"`
	}
	if err := tx.Raw(
		`SELECT permissions FROM roles WHERE id = ? AND tenant_id = ? LIMIT 1`,
		roleIDRow.RoleID, tenantID,
	).Scan(&permRow).Error; err != nil {
		return false, fmt.Errorf("goal: load role permissions for perms: %w", err)
	}
	if len(permRow.Permissions) == 0 {
		return false, nil
	}
	type permJSON struct {
		Perms []string `json:"perms"`
	}
	var pj permJSON
	if err := json.Unmarshal(permRow.Permissions, &pj); err != nil {
		return false, fmt.Errorf("goal: parse permissions json: %w", err)
	}
	for _, p := range pj.Perms {
		if p == "*" || p == "goal:read_all" || p == "goal:*" {
			return true, nil
		}
	}
	return false, nil
}

// checkGoalOwnership verifies that the actor is allowed to mutate the given
// goal.  Allowed when:
//   - actorEmployeeID is nil (unlinked admin — bypass), OR
//   - goal.EmployeeID == *actorEmployeeID (own goal), OR
//   - actor holds goal:read_all / wildcard (manager / HR admin).
//
// Must be called inside a WithinTenant transaction.
func checkGoalOwnership(tx *gorm.DB, tenantID, actorUserID uuid.UUID, actorEmployeeID *uuid.UUID, goalEmployeeID uuid.UUID) error {
	if actorEmployeeID == nil {
		// Unlinked user (e.g. system admin without an employee record) — bypass.
		return nil
	}
	if *actorEmployeeID == goalEmployeeID {
		return nil
	}
	ok, err := actorHasPrivilegedGoalAccess(tx, tenantID, actorUserID)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	return ErrForbidden
}
