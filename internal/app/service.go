// Package app 是押金托管争议服务的应用层: 所有写操作都在数据库事务内完成,
// 通过 pg_advisory_xact_lock 以"租赁(contract)"为粒度串行化, 保证
// 资金状态(escrow)/争议状态(disputes)/证据状态(evidence+findings) 同事务联动。
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"deposit-escrow/internal/domain"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/segmentio/kafka-go"
)

// 业务错误, handler 据此映射 HTTP 状态码。
var (
	ErrNotFound      = errors.New("NOT_FOUND")
	ErrConflict      = errors.New("CONFLICT")
	ErrForbidden     = errors.New("FORBIDDEN")
	ErrValidation    = errors.New("VALIDATION")
	ErrUnresolved    = errors.New("UNRESOLVED_DISPUTE")
	ErrBadState      = errors.New("BAD_STATE")
	ErrAmountExceeds = errors.New("AMOUNT_EXCEEDS_DEPOSIT")
)

type Service struct {
	pool    *pgxpool.Pool
	rdb     *redis.Client
	writer  *kafka.Writer
	topic   string
	kafkaOn bool
}

func New(pool *pgxpool.Pool, rdb *redis.Client, writer *kafka.Writer, topic string) *Service {
	return &Service{pool: pool, rdb: rdb, topic: topic, kafkaOn: writer != nil, writer: writer}
}

func (s *Service) Close() {
	if s.writer != nil {
		_ = s.writer.Close()
	}
}

// ---- 基础构件 ----

type ctxKey string

const actorKey ctxKey = "actor"

// Actor 是 domain.Actor 的别名, 保持包内现有引用不变。
type Actor = domain.Actor

func WithActor(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, actorKey, a)
}
func actor(ctx context.Context) Actor { return ctx.Value(actorKey).(Actor) }

// runTx 在合同粒度排他锁内执行事务。
func (s *Service) runTx(ctx context.Context, contractID string, fn func(pgx.Tx) error) error {
	if contractID == "" {
		return errors.New("internal: contractID required for runTx")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, contractID); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// emit 在事务内追加领域事件(事务性 outbox)。
func (s *Service) emit(tx pgx.Tx, ctx context.Context, contractID, etype string, payload any, actorID string) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO lease_events(contract_id,event_type,payload,actor_id) VALUES($1,$2,$3,NULLIF($4,'')::uuid)`,
		contractID, etype, string(b), actorID)
	return err
}

func (s *Service) notify(tx pgx.Tx, ctx context.Context, contractID, userID, template string, vars map[string]any) error {
	if vars == nil {
		vars = map[string]any{}
	}
	_, err := tx.Exec(ctx,
		`INSERT INTO notifications(contract_id,user_id,template,vars)
		 VALUES(NULLIF($1,'')::uuid,$2::uuid,$3,$4)`, contractID, userID, template, vars)
	return err
}

// ledger 追加资金台账并原子更新 escrow 四层金额。
// deltaHard/deltaDisputed/deltaUtility 可正可负; balanceDelta 仅用于入金/出金。
func (s *Service) ledger(tx pgx.Tx, ctx context.Context, contractID, txnType string,
	amount float64, balanceDelta, deltaHard, deltaDisputed, deltaUtility float64,
	refType, refID, reason, idemKey string) error {
	var bal, hard, disp, util float64
	row := tx.QueryRow(ctx, `
		UPDATE escrow_accounts
		SET balance = balance + $2,
		    hard_hold = hard_hold + $3,
		    disputed_hold = disputed_hold + $4,
		    utility_hold = utility_hold + $5,
		    version = version + 1,
		    updated_at = now()
		WHERE contract_id = $1
		RETURNING balance, hard_hold, disputed_hold, utility_hold`,
		contractID, balanceDelta, deltaHard, deltaDisputed, deltaUtility)
	if err := row.Scan(&bal, &hard, &disp, &util); err != nil {
		return fmt.Errorf("update escrow: %w", err)
	}
	var seq int64
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(MAX(seq),0)+1 FROM fund_ledger WHERE contract_id=$1`, contractID).Scan(&seq); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO fund_ledger(contract_id,seq,txn_type,amount,balance_after,hard_after,disputed_after,utility_after,
		                        ref_type,ref_id,reason,actor_id,idempotency_key)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,NULLIF($10,'')::uuid,$11,NULLIF($12,'')::uuid,NULLIF($13,''))`,
		contractID, seq, txnType, round2(amount), round2(bal), round2(hard), round2(disp), round2(util),
		refType, refID, reason, actor(ctx).ID, idemKey)
	return err
}

// assertContractParty 校验操作者是租赁关系方, 返回合同状态。
func (s *Service) assertParty(tx pgx.Tx, ctx context.Context, contractID string, roles ...string) (string, error) {
	a := actor(ctx)
	var status, tenant, landlord string
	var agent *string
	err := tx.QueryRow(ctx,
		`SELECT status, tenant_id::text, landlord_id::text, agent_id::text FROM contracts WHERE id=$1`,
		contractID).Scan(&status, &tenant, &landlord, &agent)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	party := tenant == a.ID || landlord == a.ID || (agent != nil && *agent == a.ID)
	if !party && a.Role != "PLATFORM" {
		return "", ErrForbidden
	}
	if len(roles) > 0 {
		ok := false
		for _, r := range roles {
			if r == a.Role {
				ok = true
			}
		}
		if !ok {
			return "", fmt.Errorf("%w: role %s may not perform this action", ErrForbidden, a.Role)
		}
	}
	return status, nil
}

func (s *Service) getContract(tx pgx.Tx, ctx context.Context, id string) (domain.Contract, error) {
	var c domain.Contract
	err := tx.QueryRow(ctx, `
		SELECT id::text, lease_no, property_id::text, landlord_id::text, tenant_id::text,
		       agent_id::text, start_date::text, end_date::text, rent_amount, deposit_amount,
		       COALESCE(wear_clause,''), notice_days, inspection_due_days, payment_due_days, status, created_at
		FROM contracts WHERE id=$1`, id).Scan(
		&c.ID, &c.LeaseNo, &c.PropertyID, &c.LandlordID, &c.TenantID, &c.AgentID,
		&c.StartDate, &c.EndDate, &c.RentAmount, &c.DepositAmount,
		&c.WearClause, &c.NoticeDays, &c.InspectionDueDays, &c.PaymentDueDays, &c.Status, &c.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// checkStatus 校验合同处于允许的状态之一。
func checkStatus(cur string, allowed ...string) error {
	for _, a := range allowed {
		if cur == a {
			return nil
		}
	}
	return fmt.Errorf("%w: contract is %s, allowed: %v", ErrBadState, cur, allowed)
}

// waitForKafka 供健康检查使用。
func (s *Service) KafkaOn() bool { return s.kafkaOn }

// nowUTC 便于测试时统一时区。
func nowUTC() time.Time { return time.Now().UTC() }
