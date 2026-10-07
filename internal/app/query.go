package app

import (
	"context"
	"errors"
	"fmt"

	"deposit-escrow/internal/domain"

	"github.com/jackc/pgx/v5/pgxpool"
)

// LeaseTimeline 是一条租赁的完整档案视图: 合同/托管/交割/清单/维修/验房/扣款/争议/结算/台账/事件。
type LeaseTimeline struct {
	Contract       map[string]any   `json:"contract"`
	Escrow         map[string]any   `json:"escrow"`
	Handovers      []map[string]any `json:"handovers"`
	Inventory      []map[string]any `json:"inventory"`
	Repairs        []map[string]any `json:"repairs"`
	Inspection     map[string]any   `json:"inspection"`
	Findings       []map[string]any `json:"findings"`
	Charges        []map[string]any `json:"charges"`
	Disputes       []map[string]any `json:"disputes"`
	Settlement     map[string]any   `json:"settlement"`
	UtilityReserve map[string]any   `json:"utility_reserve"`
	Ledger         []map[string]any `json:"fund_ledger"`
	Events         []map[string]any `json:"events"`
	Actions        []map[string]any `json:"dispute_actions"`
	OpenDisputes   int              `json:"open_disputes"`
	Available      float64          `json:"available_to_release"`
}

type Queries struct{ pool *pgxpool.Pool }

func NewQueries(pool *pgxpool.Pool) *Queries { return &Queries{pool: pool} }

// AssertContractParty 校验 actor 是该租赁的参与方(平台放行)。
func (q *Queries) AssertContractParty(ctx context.Context, contractID string, actor domain.Actor) error {
	if actor.Role == "PLATFORM" {
		return nil
	}
	var n int
	err := q.pool.QueryRow(ctx, `
		SELECT count(*) FROM contracts
		WHERE id=$1 AND (tenant_id=$2 OR landlord_id=$2 OR agent_id=$2)`,
		contractID, actor.ID).Scan(&n)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrForbidden
	}
	return nil
}

func (q *Queries) rows(ctx context.Context, query string, args ...any) ([]map[string]any, error) {
	rows, err := q.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	fields := rows.FieldDescriptions()
	var out []map[string]any
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return nil, err
		}
		m := map[string]any{}
		for i, f := range fields {
			m[string(f.Name)] = vals[i]
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (q *Queries) row(ctx context.Context, query string, args ...any) (map[string]any, error) {
	rs, err := q.rows(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	if len(rs) == 0 {
		return nil, ErrNotFound
	}
	return rs[0], nil
}

func (q *Queries) LeaseTimeline(ctx context.Context, contractID string) (*LeaseTimeline, error) {
	t := &LeaseTimeline{}
	var err error
	if t.Contract, err = q.row(ctx, `SELECT * FROM contracts WHERE id=$1`, contractID); err != nil {
		return nil, err
	}
	if t.Escrow, err = q.row(ctx, `SELECT * FROM escrow_accounts WHERE contract_id=$1`, contractID); err != nil {
		if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
		t.Escrow = map[string]any{}
	}
	if t.Handovers, err = q.rows(ctx, `SELECT * FROM handovers WHERE contract_id=$1 ORDER BY occurred_at`, contractID); err != nil {
		return nil, err
	}
	if t.Inventory, err = q.rows(ctx, `SELECT * FROM inventory_items WHERE contract_id=$1 ORDER BY created_at`, contractID); err != nil {
		return nil, err
	}
	if t.Repairs, err = q.rows(ctx, `SELECT * FROM repairs WHERE contract_id=$1 ORDER BY occurred_at`, contractID); err != nil {
		return nil, err
	}
	if t.Inspection, err = q.row(ctx, `SELECT * FROM inspections WHERE contract_id=$1`, contractID); err != nil {
		t.Inspection = map[string]any{}
	}
	if t.Findings, err = q.rows(ctx, `SELECT * FROM findings WHERE contract_id=$1 ORDER BY created_at`, contractID); err != nil {
		return nil, err
	}
	if t.Charges, err = q.rows(ctx, `SELECT * FROM charge_items WHERE contract_id=$1 ORDER BY created_at`, contractID); err != nil {
		return nil, err
	}
	if t.Disputes, err = q.rows(ctx, `SELECT * FROM disputes WHERE contract_id=$1 ORDER BY created_at`, contractID); err != nil {
		return nil, err
	}
	if t.Settlement, err = q.row(ctx, `SELECT * FROM settlements WHERE contract_id=$1`, contractID); err != nil {
		t.Settlement = map[string]any{}
	}
	t.UtilityReserve = q.utilityReserveView(ctx, contractID)
	if t.Ledger, err = q.rows(ctx, `
		SELECT seq,txn_type,amount,balance_after,hard_after,disputed_after,utility_after,ref_type,reason,created_at
		FROM fund_ledger WHERE contract_id=$1 ORDER BY seq`, contractID); err != nil {
		return nil, err
	}
	if t.Events, err = q.rows(ctx, `
		SELECT event_id,event_type,payload,actor_id,published,created_at
		FROM lease_events WHERE contract_id=$1 ORDER BY id`, contractID); err != nil {
		return nil, err
	}
	if t.Actions, err = q.rows(ctx, `
		SELECT da.* FROM dispute_actions da JOIN disputes d ON d.id=da.dispute_id
		WHERE da.contract_id=$1 ORDER BY da.id`, contractID); err != nil {
		return nil, err
	}
	if n, ok := t.Escrow["hard_hold"]; ok {
		bal := asFloat(t.Escrow["balance"])
		hard := asFloat(n)
		disp := asFloat(t.Escrow["disputed_hold"])
		util := asFloat(t.Escrow["utility_hold"])
		t.Available = round2(bal - hard - disp - util)
	}
	if r, e := q.row(ctx, `SELECT count(*)::int AS n FROM disputes WHERE contract_id=$1
		AND status IN ('OPEN','EVIDENCE_PENDING','UNDER_REVIEW')`, contractID); e == nil {
		t.OpenDisputes = int(asFloat(r["n"]))
	}
	return t, nil
}

func (q *Queries) ListContracts(ctx context.Context, role, userID string, limit int) ([]map[string]any, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if role == "PLATFORM" {
		return q.rows(ctx, `SELECT id::text,lease_no,status,tenant_id::text,landlord_id::text,
			deposit_amount,start_date,end_date,created_at FROM contracts ORDER BY created_at DESC LIMIT $1`, limit)
	}
	return q.rows(ctx, `SELECT id::text,lease_no,status,tenant_id::text,landlord_id::text,
		deposit_amount,start_date,end_date,created_at FROM contracts
		WHERE tenant_id=$1 OR landlord_id=$1 OR agent_id=$1
		ORDER BY created_at DESC LIMIT $2`, userID, limit)
}

// utilityReserveView 聚合水电尾款预留(主单+分项+账单+照片)。无预留时返回空 map。
func (q *Queries) utilityReserveView(ctx context.Context, contractID string) map[string]any {
	r, err := q.row(ctx, `SELECT * FROM utility_reservations WHERE contract_id=$1`, contractID)
	if err != nil || r == nil {
		return map[string]any{}
	}
	if items, e := q.rows(ctx,
		`SELECT * FROM utility_reserve_items WHERE reservation_id=$1 ORDER BY kind`, r["id"]); e == nil {
		r["items"] = items
	} else {
		r["items"] = []any{}
	}
	if bills, e := q.rows(ctx,
		`SELECT id,provider,bill_no,kind,period_start,period_end,billed_reading,amount,created_at
		 FROM utility_bills WHERE contract_id=$1 ORDER BY created_at`, contractID); e == nil {
		r["bills"] = bills
	} else {
		r["bills"] = []any{}
	}
	if photos, e := q.rows(ctx,
		`SELECT id,kind,reading,reading_from,taken_at,evidence_id,uploaded_by,created_at
		 FROM utility_moveout_photos WHERE contract_id=$1 ORDER BY created_at`, contractID); e == nil {
		r["photos"] = photos
	} else {
		r["photos"] = []any{}
	}
	return r
}

func asFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case float32:
		return float64(x)
	case int64:
		return float64(x)
	case int:
		return float64(x)
	case nil:
		return 0
	default:
		f := 0.0
		_, _ = fmt.Sscanf(fmt.Sprint(x), "%f", &f)
		return f
	}
}
