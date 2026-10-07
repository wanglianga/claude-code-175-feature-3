package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"deposit-escrow/internal/domain"

	"github.com/jackc/pgx/v5"
)

// 退租水电尾款结算用例。
//
// 资金在 escrow 第四层 utility_hold 中圈定: 租客搬离、最后一期供应商账单尚未生成时,
// 平台依据最近读数与历史用量预留; 账单到达后与租客搬离当天表计照片一起复核(见 utility_review.go),
// 多退少补; 账单延迟/读数无法对应/分摊争议时保持待处理并进入人工复核。
// 同一条租赁只可能产生一笔 UTILITY_FINAL 最终押金流水(DB 唯一索引兜底)。

// 默认预计结算周期(自搬离之日起 N 天)与读数比对容差。
const (
	defaultExpectedSettleDays = 30
	readingTolerance          = 1.0
)

// ---------- 创建水电尾款预留 ----------

type CreateUtilityReserveInput struct {
	ExpectedSettleDays int      `json:"expected_settle_days"`
	Kinds              []string `json:"kinds"` // 缺省取全部有退租读数的品类
	Note               string   `json:"note"`
}

type kindEstimate struct {
	Kind            string
	CheckinReading  float64
	LastReading     float64
	UnitPrice       float64
	HistoryAvgUsage float64
	EstAmount       float64
	LastReadingAt   *time.Time
}

// CreateUtilityReserve 在租客搬离、最后一期水电账单未生成时预留部分押金。
func (s *Service) CreateUtilityReserve(ctx context.Context, contractID string,
	in CreateUtilityReserveInput) (string, float64, error) {
	var reserveID string
	var total float64
	err := s.runTx(ctx, contractID, func(tx pgx.Tx) error {
		if _, err := s.assertParty(tx, ctx, contractID, "LANDLORD", "AGENT", "PLATFORM"); err != nil {
			return err
		}
		var cst string
		if err := tx.QueryRow(ctx, `SELECT status FROM contracts WHERE id=$1`, contractID).Scan(&cst); err != nil {
			return err
		}
		if err := checkStatus(cst, domain.StatusCheckoutPending, domain.StatusEarlyTerminated,
			domain.StatusInspecting, domain.StatusDeductionProposed); err != nil {
			return err
		}
		var exists bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM utility_reservations WHERE contract_id=$1)`, contractID).
			Scan(&exists); err != nil {
			return err
		}
		if exists {
			return fmt.Errorf("%w: 水电尾款预留已存在, 不可重复创建", ErrConflict)
		}
		// 已按验房定论(hard_hold)扣过水电的, 不能再预留, 避免双重扣款。
		var settledUtil float64
		if err := tx.QueryRow(ctx, `
			SELECT COALESCE(sum(amount),0) FROM charge_items
			WHERE contract_id=$1 AND kind='UTILITY_UNPAID'
			  AND status IN ('ACCEPTED','UPHELD')`, contractID).Scan(&settledUtil); err != nil {
			return err
		}
		if settledUtil > 0 {
			return fmt.Errorf("%w: 水电费用已在验房扣款中定论 %.2f 元, 不能再预留", ErrConflict, settledUtil)
		}

		estimates, err := s.loadReserveEstimates(tx, ctx, contractID, in.Kinds)
		if err != nil {
			return err
		}
		if len(estimates) == 0 {
			return fmt.Errorf("%w: 缺少退租水电读数, 无法估算预留", ErrValidation)
		}
		snapshot := map[string]any{}
		for _, e := range estimates {
			total += e.EstAmount
			snapshot[e.Kind] = map[string]any{
				"last_reading": e.LastReading, "history_avg_usage": e.HistoryAvgUsage,
				"unit_price": e.UnitPrice, "est_amount": e.EstAmount,
			}
		}
		total = round2(total)
		if total <= 0 {
			return fmt.Errorf("%w: 估算预留金额为 0(缺少单价或用量), 不能预留", ErrValidation)
		}
		var available float64
		if err := tx.QueryRow(ctx,
			`SELECT balance - hard_hold - disputed_hold - utility_hold
			 FROM escrow_accounts WHERE contract_id=$1`, contractID).Scan(&available); err != nil {
			return err
		}
		if total > available+0.001 {
			return fmt.Errorf("%w: 需预留 %.2f 元, 可退余额仅 %.2f 元", ErrAmountExceeds, total, round2(available))
		}

		// 预计结算时间: 搬离交割日 + N 天。
		var checkoutAt time.Time
		if err := tx.QueryRow(ctx, `
			SELECT occurred_at FROM handovers WHERE contract_id=$1 AND phase='CHECKOUT'`,
			contractID).Scan(&checkoutAt); err != nil {
			return err
		}
		days := in.ExpectedSettleDays
		if days <= 0 {
			days = defaultExpectedSettleDays
		}
		expectedAt := checkoutAt.AddDate(0, 0, days)

		// 若验房已生成"水电欠费"PROPOSED 建议, 由本预留接管, 撤销其软冻结。
		var proposedUtil float64
		if err := tx.QueryRow(ctx, `
			SELECT COALESCE(sum(amount),0) FROM charge_items
			WHERE contract_id=$1 AND kind='UTILITY_UNPAID' AND status='PROPOSED'`,
			contractID).Scan(&proposedUtil); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			DELETE FROM charge_items WHERE contract_id=$1 AND kind='UTILITY_UNPAID' AND status='PROPOSED'`,
			contractID); err != nil {
			return err
		}

		reason := fmt.Sprintf(
			"租客已搬离, 最后一期水/电/燃气供应商账单尚未生成; 按搬离最近读数与历史月均用量预留 %d 天费用",
			days)
		snap, _ := json.Marshal(snapshot)
		if err := tx.QueryRow(ctx, `
			INSERT INTO utility_reservations(contract_id,status,reserve_amount,reason,basis_snapshot,
			                                 expected_settle_at,created_by)
			VALUES($1,'RESERVED',$2,$3,$4,$5,$6) RETURNING id::text`,
			contractID, total, reason, string(snap), expectedAt, actor(ctx).ID).Scan(&reserveID); err != nil {
			return fmt.Errorf("create utility reserve: %w", err)
		}
		for _, e := range estimates {
			if _, err := tx.Exec(ctx, `
				INSERT INTO utility_reserve_items(reservation_id,kind,last_reading,last_reading_at,
				                                  history_avg_usage,unit_price,est_amount)
				VALUES($1,$2,$3,$4,$5,$6,$7)`,
				reserveID, e.Kind, e.LastReading, e.LastReadingAt, e.HistoryAvgUsage,
				e.UnitPrice, e.EstAmount); err != nil {
				return err
			}
		}
		// 预留接管验房水电建议: 先释放对应 disputed_hold, 再圈定 utility_hold。
		if proposedUtil > 0 {
			if err := s.ledger(tx, ctx, contractID, "HOLD_RELEASE", proposedUtil, 0, 0, -proposedUtil, 0,
				"utility_reserve", reserveID, "水电欠费由尾款预留接管, 撤销验房建议冻结", ""); err != nil {
				return err
			}
		}
		if err := s.ledger(tx, ctx, contractID, "UTILITY_RESERVE", total, 0, 0, 0, total,
			"utility_reserve", reserveID, reason, "utility-reserve:"+contractID); err != nil {
			return err
		}
		c, _ := s.getContract(tx, ctx, contractID)
		_ = s.notify(tx, ctx, contractID, c.TenantID, "utility_reserved",
			map[string]any{"amount": total, "expected_settle_at": expectedAt.Format("2006-01-02"), "reason": reason})
		_ = s.notify(tx, ctx, contractID, c.LandlordID, "utility_reserved",
			map[string]any{"amount": total, "expected_settle_at": expectedAt.Format("2006-01-02"), "reason": reason})
		return s.emit(tx, ctx, contractID, "UtilityReserveCreated", map[string]any{
			"reservation_id": reserveID, "reserve_amount": total,
			"expected_settle_at": expectedAt.Format("2006-01-02"), "reason": reason,
		}, actor(ctx).ID)
	})
	return reserveID, total, err
}

// loadReserveEstimates 从入住/退租读数与合同期限估算各品类预留。
func (s *Service) loadReserveEstimates(tx pgx.Tx, ctx context.Context, contractID string,
	kinds []string) ([]kindEstimate, error) {
	want := map[string]bool{}
	for _, k := range kinds {
		want[k] = true
	}
	var checkinAt, checkoutAt time.Time
	err := tx.QueryRow(ctx, `
		SELECT h1.occurred_at, h2.occurred_at
		FROM handovers h1 JOIN handovers h2 ON h2.contract_id=h1.contract_id
		WHERE h1.contract_id=$1 AND h1.phase='CHECKIN' AND h2.phase='CHECKOUT'`,
		contractID).Scan(&checkinAt, &checkoutAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: 需先完成入住与退租交割", ErrBadState)
	}
	if err != nil {
		return nil, err
	}
	tenureDays := int(checkoutAt.Sub(checkinAt).Hours() / 24)
	if tenureDays < 1 {
		tenureDays = 1
	}
	rows, err := tx.Query(ctx, `
		SELECT ci.kind, ci.reading, COALESCE(NULLIF(co.unit_price,0),ci.unit_price),
		       co.reading, co.recorded_at
		FROM utility_readings ci
		JOIN utility_readings co ON co.contract_id=ci.contract_id AND co.kind=ci.kind
		WHERE ci.contract_id=$1 AND ci.phase='CHECKIN' AND co.phase='CHECKOUT'`, contractID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []kindEstimate
	for rows.Next() {
		var e kindEstimate
		if err := rows.Scan(&e.Kind, &e.CheckinReading, &e.UnitPrice, &e.LastReading, &e.LastReadingAt); err != nil {
			return nil, err
		}
		if len(want) > 0 && !want[e.Kind] {
			continue
		}
		// 月均用量按至少一个月折算, 避免入住/退租同日录入时用量被放大。
		avg := (e.LastReading - e.CheckinReading)
		if avg < 0 {
			avg = 0
		}
		months := float64(tenureDays) / 30.0
		if months < 1 {
			months = 1
		}
		avg = avg / months
		e.HistoryAvgUsage = round2(avg)
		e.EstAmount = round2(avg * e.UnitPrice)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---------- 租客提交搬离当天水表/电表照片 ----------

type MoveoutPhotoInput struct {
	Kind       string  `json:"kind"`
	Reading    float64 `json:"reading"`
	TakenAt    string  `json:"taken_at"` // RFC3339, 缺省取当前时间
	EvidenceID string  `json:"evidence_id"`
	Note       string  `json:"note"`
}

func (s *Service) SubmitMoveoutPhoto(ctx context.Context, contractID string,
	in MoveoutPhotoInput) (string, error) {
	var photoID string
	err := s.runTx(ctx, contractID, func(tx pgx.Tx) error {
		if _, err := s.assertParty(tx, ctx, contractID, "TENANT", "PLATFORM"); err != nil {
			return err
		}
		if in.Kind != "WATER" && in.Kind != "ELECTRIC" && in.Kind != "GAS" {
			return fmt.Errorf("%w: kind 必须是 WATER/ELECTRIC/GAS", ErrValidation)
		}
		if in.EvidenceID == "" {
			return fmt.Errorf("%w: 必须上传搬离当天表计照片(evidence_id)", ErrValidation)
		}
		var rid, st string
		if err := tx.QueryRow(ctx,
			`SELECT id::text, status FROM utility_reservations WHERE contract_id=$1`,
			contractID).Scan(&rid, &st); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: 尚未创建水电尾款预留", ErrBadState)
			}
			return err
		}
		// 证据归属校验
		var ok bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM evidence WHERE id=$1::uuid AND contract_id=$2)`,
			in.EvidenceID, contractID).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: 证据不存在或不属于本租赁", ErrValidation)
		}
		// 同一证据重复提交: 幂等, 即使已最终结算也直接回显, 不产生任何资金变化。
		var existing string
		if e := tx.QueryRow(ctx, `SELECT id::text FROM utility_moveout_photos
			WHERE reservation_id=$1 AND evidence_id=$2::uuid`, rid, in.EvidenceID).Scan(&existing); e == nil {
			photoID = existing
			return nil
		} else if !errors.Is(e, pgx.ErrNoRows) {
			return e
		}
		if st == domain.UtilityReserveFinalized {
			return fmt.Errorf("%w: 已最终结算, 不能再补传不同照片(最终结果保持唯一)", ErrBadState)
		}
		taken := nowUTC()
		if in.TakenAt != "" {
			if t, perr := time.Parse(time.RFC3339, in.TakenAt); perr == nil {
				taken = t
			}
		}
		// 按入住读数与单价推算照片口径费用。
		var photoAmt, checkinReading, price float64
		if err := tx.QueryRow(ctx, `
			SELECT ci.reading, COALESCE(NULLIF(ri.unit_price,0),ci.unit_price)
			FROM utility_reserve_items ri
			JOIN utility_readings ci ON ci.contract_id=$2 AND ci.phase='CHECKIN' AND ci.kind=ri.kind
			WHERE ri.reservation_id=$1 AND ri.kind=$3`, rid, contractID, in.Kind).
			Scan(&checkinReading, &price); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: 预留中没有 %s 品类", ErrValidation, in.Kind)
			}
			return err
		}
		if in.Reading+readingTolerance < checkinReading {
			return fmt.Errorf("%w: 照片读数 %.2f 低于入住读数 %.2f", ErrValidation, in.Reading, checkinReading)
		}
		photoAmt = round2((in.Reading - checkinReading) * price)

		if err := tx.QueryRow(ctx, `
			INSERT INTO utility_moveout_photos(contract_id,reservation_id,kind,reading,taken_at,
			                                   evidence_id,note,uploaded_by)
			VALUES($2,$1,$3,$4,$5,$6::uuid,$7,$8)
			ON CONFLICT (reservation_id, kind) DO UPDATE
			  SET reading=EXCLUDED.reading, taken_at=EXCLUDED.taken_at, evidence_id=EXCLUDED.evidence_id,
			      note=EXCLUDED.note, uploaded_by=EXCLUDED.uploaded_by, created_at=now()
			RETURNING id::text`,
			rid, contractID, in.Kind, in.Reading, taken, in.EvidenceID, in.Note,
			actor(ctx).ID).Scan(&photoID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE utility_reserve_items SET photo_reading=$2, photo_amount=$3, review_flag='OK'
			WHERE reservation_id=$1 AND kind=$4`, rid, in.Reading, photoAmt, in.Kind); err != nil {
			return err
		}
		c, _ := s.getContract(tx, ctx, contractID)
		_ = s.notify(tx, ctx, contractID, c.LandlordID, "utility_photo_submitted",
			map[string]any{"kind": in.Kind, "reading": in.Reading})
		if err := s.emit(tx, ctx, contractID, "UtilityMoveoutPhotoSubmitted", map[string]any{
			"reservation_id": rid, "kind": in.Kind, "reading": in.Reading,
			"photo_amount": photoAmt, "evidence_id": in.EvidenceID,
		}, actor(ctx).ID); err != nil {
			return err
		}
		// 照片补齐后重新复核: 账单已齐且读数一致则自动最终结算; 读数不符转人工复核。
		decision, derr := s.reviewReserve(tx, ctx, contractID, rid)
		if derr != nil {
			return derr
		}
		if decision.Blocker == "" {
			return s.finalizeUtilityReserve(tx, ctx, contractID, rid, decision, "",
				"搬离照片补齐, 与供应商账单复核一致, 自动最终结算")
		}
		if decision.Blocker == domain.UtilityFlagReadingMismatch {
			if _, err := tx.Exec(ctx,
				`UPDATE utility_reservations SET status='MANUAL_REVIEW' WHERE id=$1`, rid); err != nil {
				return err
			}
			return s.emit(tx, ctx, contractID, "UtilityReserveManualReview", map[string]any{
				"reservation_id": rid, "blocker": decision.Blocker, "notes": decision.Notes,
			}, actor(ctx).ID)
		}
		return nil
	})
	return photoID, err
}

// ---------- 供应商账单到达(幂等) + 自动复核 ----------

type UtilityBillInput struct {
	Provider        string  `json:"provider"`
	BillNo          string  `json:"bill_no"`
	Kind            string  `json:"kind"`
	PeriodStart     string  `json:"period_start"`
	PeriodEnd       string  `json:"period_end"`
	BilledReading   float64 `json:"billed_reading"`
	Amount          float64 `json:"amount"`
	InvoiceEvidence string  `json:"invoice_evidence_id"`
}

type UtilityBillResult struct {
	BillID         string   `json:"bill_id"`
	Reused         bool     `json:"reused"`
	ReservationID  string   `json:"reservation_id"`
	Status         string   `json:"status"`
	Finalized      bool     `json:"finalized"`
	FinalAmount    float64  `json:"final_amount"`
	RefundAmount   float64  `json:"refund_amount"`
	ShortageAmount float64  `json:"shortage_amount"`
	Blocker        string   `json:"blocker,omitempty"`
	Notes          []string `json:"notes,omitempty"`
}

// ReceiveUtilityBill 登记供应商最后一期账单; 重复账单号直接幂等返回。
// 登记后立即把账单与搬离照片读数一起复核: 数据齐备且一致则同事务最终结算。
func (s *Service) ReceiveUtilityBill(ctx context.Context, contractID string,
	in UtilityBillInput) (*UtilityBillResult, error) {
	res := &UtilityBillResult{}
	err := s.runTx(ctx, contractID, func(tx pgx.Tx) error {
		if _, err := s.assertParty(tx, ctx, contractID, "LANDLORD", "AGENT", "PLATFORM"); err != nil {
			return err
		}
		if in.BillNo == "" {
			return fmt.Errorf("%w: bill_no 必填(用于账单幂等)", ErrValidation)
		}
		if in.Kind != "WATER" && in.Kind != "ELECTRIC" && in.Kind != "GAS" {
			return fmt.Errorf("%w: kind 必须是 WATER/ELECTRIC/GAS", ErrValidation)
		}
		if in.Amount < 0 {
			return fmt.Errorf("%w: 金额不能为负", ErrValidation)
		}
		if in.Provider == "" {
			in.Provider = "SUPPLIER"
		}
		var rid, st string
		if err := tx.QueryRow(ctx,
			`SELECT id::text, status FROM utility_reservations WHERE contract_id=$1`,
			contractID).Scan(&rid, &st); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: 尚未创建水电尾款预留", ErrBadState)
			}
			return err
		}
		res.ReservationID = rid
		// 账单幂等优先: 即使已最终结算, 重复账单也只回显, 绝不触发第二次扣款。
		var existingID string
		err := tx.QueryRow(ctx, `
			SELECT id::text FROM utility_bills WHERE contract_id=$1 AND provider=$2 AND bill_no=$3`,
			contractID, in.Provider, in.BillNo).Scan(&existingID)
		if err == nil {
			res.BillID, res.Reused, res.Status = existingID, true, st
			return nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if st == domain.UtilityReserveFinalized {
			return fmt.Errorf("%w: 已最终结算, 新账单不能再次扣款(如需更正请走人工复核)", ErrBadState)
		}

		var pStart, pEnd *time.Time
		if in.PeriodStart != "" {
			if t, e := time.Parse("2006-01-02", in.PeriodStart); e == nil {
				pStart = &t
			}
		}
		if in.PeriodEnd != "" {
			if t, e := time.Parse("2006-01-02", in.PeriodEnd); e == nil {
				pEnd = &t
			}
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO utility_bills(contract_id,reservation_id,provider,bill_no,kind,period_start,
			                          period_end,billed_reading,amount,invoice_evidence_id,received_by)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,NULLIF($10,'')::uuid,$11) RETURNING id::text`,
			contractID, rid, in.Provider, in.BillNo, in.Kind, pStart, pEnd,
			in.BilledReading, in.Amount, in.InvoiceEvidence, actor(ctx).ID).Scan(&res.BillID); err != nil {
			return fmt.Errorf("register utility bill: %w", err)
		}
		// 回填该品类账单读数与金额(同品类更正账单以最新一份为准)。
		if _, err := tx.Exec(ctx, `
			UPDATE utility_reserve_items SET billed_reading=$2, billed_amount=$3
			WHERE reservation_id=$1 AND kind=$4`, rid, in.BilledReading, in.Amount, in.Kind); err != nil {
			return err
		}

		decision, err := s.reviewReserve(tx, ctx, contractID, rid)
		if err != nil {
			return err
		}
		res.Notes = decision.Notes
		if decision.Blocker == "" {
			if err := s.finalizeUtilityReserve(tx, ctx, contractID, rid, decision, "", ""); err != nil {
				return err
			}
			res.Finalized = true
			res.FinalAmount = decision.FinalTotal
			res.RefundAmount, res.ShortageAmount, _, _ = s.reserveMoneyOutcome(tx, ctx, rid)
		} else {
			newSt := domain.UtilityReserveReviewPending
			etype := "UtilityBillDelayed"
			if decision.Blocker == domain.UtilityFlagReadingMismatch ||
				decision.Blocker == domain.UtilityFlagSplitDispute {
				newSt = domain.UtilityReserveManualReview
				etype = "UtilityReserveManualReview"
			}
			if _, err := tx.Exec(ctx, `UPDATE utility_reservations SET status=$2 WHERE id=$1`,
				rid, newSt); err != nil {
				return err
			}
			res.Blocker = decision.Blocker
			if err := s.emit(tx, ctx, contractID, etype, map[string]any{
				"reservation_id": rid, "blocker": decision.Blocker, "notes": decision.Notes,
			}, actor(ctx).ID); err != nil {
				return err
			}
		}
		res.Status, _ = s.reserveStatus(tx, ctx, rid)
		return s.emit(tx, ctx, contractID, "UtilityBillReceived", map[string]any{
			"reservation_id": rid, "bill_id": res.BillID, "bill_no": in.BillNo,
			"kind": in.Kind, "amount": in.Amount, "finalized": res.Finalized,
			"blocker": res.Blocker,
		}, actor(ctx).ID)
	})
	return res, err
}

// ---------- 分摊争议 ----------

type UtilityDisputeInput struct {
	Kind          string   `json:"kind"`
	Subject       string   `json:"subject"`
	Comment       string   `json:"comment"`
	ClaimedAmount float64  `json:"claimed_amount"`
	EvidenceIDs   []string `json:"evidence_ids"`
}

// OpenUtilityDispute 双方就水电费用分摊/读数对应打开争议, 预留款立即转人工复核挂起。
func (s *Service) OpenUtilityDispute(ctx context.Context, contractID string,
	in UtilityDisputeInput) (string, error) {
	var disputeID string
	err := s.runTx(ctx, contractID, func(tx pgx.Tx) error {
		if _, err := s.assertParty(tx, ctx, contractID, "TENANT", "LANDLORD", "AGENT", "PLATFORM"); err != nil {
			return err
		}
		var rid, st string
		if err := tx.QueryRow(ctx,
			`SELECT id::text, status FROM utility_reservations WHERE contract_id=$1`,
			contractID).Scan(&rid, &st); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: 尚未创建水电尾款预留", ErrBadState)
			}
			return err
		}
		if st == domain.UtilityReserveFinalized {
			return fmt.Errorf("%w: 已最终结算", ErrBadState)
		}
		var openN int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM disputes WHERE utility_reserve_id=$1
			  AND status IN ('OPEN','EVIDENCE_PENDING','UNDER_REVIEW')`, rid).Scan(&openN); err != nil {
			return err
		}
		if openN > 0 {
			return fmt.Errorf("%w: 该水电预留已有未闭合争议", ErrConflict)
		}
		subject := in.Subject
		if subject == "" {
			subject = "水电尾款费用分摊/读数争议"
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO disputes(contract_id,reason,subject,claimed_amount,utility_reserve_id,
			                     opened_by,status)
			VALUES($1,$2,$3,$4,$5::uuid,$6,'OPEN') RETURNING id::text`,
			contractID, domain.UtilityFinalDisputeReason, subject, in.ClaimedAmount,
			rid, actor(ctx).ID).Scan(&disputeID); err != nil {
			return fmt.Errorf("open utility dispute: %w", err)
		}
		if in.Kind != "" {
			if _, err := tx.Exec(ctx, `
				UPDATE utility_reserve_items SET review_flag='SPLIT_DISPUTE'
				WHERE reservation_id=$1 AND kind=$2`, rid, in.Kind); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx,
			`UPDATE utility_reservations SET status='MANUAL_REVIEW' WHERE id=$1`, rid); err != nil {
			return err
		}
		if err := s.disputeAction(tx, ctx, disputeID, contractID, "OPEN",
			in.ClaimedAmount, in.EvidenceIDs, in.Comment); err != nil {
			return err
		}
		c, _ := s.getContract(tx, ctx, contractID)
		other := c.LandlordID
		if actor(ctx).ID == c.LandlordID {
			other = c.TenantID
		}
		_ = s.notify(tx, ctx, contractID, other, "utility_dispute_opened",
			map[string]any{"dispute_id": disputeID, "subject": subject})
		return s.emit(tx, ctx, contractID, "DisputeOpened", map[string]any{
			"dispute_id": disputeID, "reason": domain.UtilityFinalDisputeReason,
			"reservation_id": rid, "claimed_amount": in.ClaimedAmount,
		}, actor(ctx).ID)
	})
	return disputeID, err
}

// ---------- 平台人工复核定论 ----------

type ManualUtilityFinalizeInput struct {
	Amounts map[string]float64 `json:"amounts"` // 各品类最终费用; 缺省品类按 0 或账单
	Note    string             `json:"note"`
}

// ManualFinalizeUtilityReserve 平台在账单延迟/读数不符/分摊争议闭合后人工定论最终费用。
func (s *Service) ManualFinalizeUtilityReserve(ctx context.Context, contractID string,
	in ManualUtilityFinalizeInput) error {
	return s.runTx(ctx, contractID, func(tx pgx.Tx) error {
		a := actor(ctx)
		if a.Role != "PLATFORM" {
			return fmt.Errorf("%w: 仅平台可人工复核定论", ErrForbidden)
		}
		var rid, st string
		if err := tx.QueryRow(ctx,
			`SELECT id::text, status FROM utility_reservations WHERE contract_id=$1`,
			contractID).Scan(&rid, &st); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if st == domain.UtilityReserveFinalized {
			return fmt.Errorf("%w: 已最终结算, 不能重复定论", ErrBadState)
		}
		var openN int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM disputes WHERE utility_reserve_id=$1
			  AND status IN ('OPEN','EVIDENCE_PENDING','UNDER_REVIEW')`, rid).Scan(&openN); err != nil {
			return err
		}
		if openN > 0 {
			return fmt.Errorf("%w: 仍有 %d 个水电争议未闭合, 请先裁决", ErrUnresolved, openN)
		}
		decision, err := s.reviewReserve(tx, ctx, contractID, rid)
		if err != nil {
			return err
		}
		// 平台显式给值覆盖自动复核金额。
		for k, v := range in.Amounts {
			if v < 0 {
				return fmt.Errorf("%w: %s 金额不能为负", ErrValidation, k)
			}
			decision.FinalAmounts[k] = round2(v)
			if flag, ok := decision.Flags[k]; ok && flag == domain.UtilityFlagReadingMismatch ||
				flag == domain.UtilityFlagBillDelay {
				decision.Flags[k] = domain.UtilityFlagOK
			}
		}
		total := 0.0
		for _, v := range decision.FinalAmounts {
			total += v
		}
		decision.FinalTotal = round2(total)
		decision.Blocker = ""
		decision.Decision = "MANUAL"
		return s.finalizeUtilityReserve(tx, ctx, contractID, rid, decision, "MANUAL", in.Note)
	})
}

// ---------- 租客补缴 ----------

// PayUtilityTopup 实际费用超过预留时, 租客把不足部分补入托管。仅允许补到 shortage 为止。
func (s *Service) PayUtilityTopup(ctx context.Context, contractID string) (float64, error) {
	var pay float64
	err := s.runTx(ctx, contractID, func(tx pgx.Tx) error {
		if _, err := s.assertParty(tx, ctx, contractID, "TENANT", "PLATFORM"); err != nil {
			return err
		}
		var rid string
		var shortage, received float64
		if err := tx.QueryRow(ctx, `
			SELECT id::text, shortage_amount, topup_received
			FROM utility_reservations WHERE contract_id=$1 AND status='FINALIZED'`,
			contractID).Scan(&rid, &shortage, &received); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: 尚无待补缴的最终结算", ErrBadState)
			}
			return err
		}
		pay = round2(shortage - received)
		if pay <= 0 {
			return fmt.Errorf("%w: 没有待补缴金额", ErrBadState)
		}
		// 入金并直接计入定论扣款(最终将付给房东)。
		if err := s.ledger(tx, ctx, contractID, "UTILITY_TOPUP", pay, pay, pay, 0, 0,
			"utility_reserve", rid, "水电尾款不足部分租客补缴", "utility-topup:"+contractID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE utility_reservations SET topup_received = topup_received + $2 WHERE id=$1`,
			rid, pay); err != nil {
			return err
		}
		c, _ := s.getContract(tx, ctx, contractID)
		_ = s.notify(tx, ctx, contractID, c.LandlordID, "utility_topup_received",
			map[string]any{"amount": pay})
		return s.emit(tx, ctx, contractID, "UtilityTopupReceived", map[string]any{
			"reservation_id": rid, "amount": pay, "shortage_amount": shortage,
		}, actor(ctx).ID)
	})
	return pay, err
}

// ---------- 复核与最终结算(事务内) ----------

// resolveUtilityDispute 闭合水电尾款预留争议并联动资金。
//   - WITHDRAW: 撤回申诉, 若账单与照片齐备且一致则自动最终结算, 否则继续挂账;
//   - PARTIES_AGREE / ADJUDICATE: 以双方/平台定论的总费用结算(按各品类账单或估算权重分摊)。
func (s *Service) resolveUtilityDispute(tx pgx.Tx, ctx context.Context,
	contractID, disputeID, reservationID, resolveStatus, note string,
	in DisputeActionInput) error {
	if in.Action == "WITHDRAW" {
		if _, err := tx.Exec(ctx, `
			UPDATE disputes SET status='RESOLVED_AGREEMENT', resolution_note=$2, resolved_at=now()
			WHERE id=$1`, disputeID, "申诉撤回: "+note); err != nil {
			return err
		}
		if err := s.disputeAction(tx, ctx, disputeID, contractID, "WITHDRAW", 0,
			in.EvidenceIDs, in.Comment); err != nil {
			return err
		}
		if err := s.emit(tx, ctx, contractID, "DisputeResolved", map[string]any{
			"dispute_id": disputeID, "reservation_id": reservationID,
			"resolution": "WITHDRAWN",
		}, actor(ctx).ID); err != nil {
			return err
		}
		return s.recomputeReserveAfterDispute(tx, ctx, contractID, reservationID)
	}

	award := round2(in.Amount)
	if award < 0 {
		return fmt.Errorf("%w: 定论水电费用不能为负", ErrValidation)
	}
	if resolveStatus == "RESOLVED_ADJUDICATION" {
		if _, err := tx.Exec(ctx, `
			UPDATE disputes SET status=$2, award_to_landlord=$3, resolution_note=$4, resolved_at=now(),
			                    adjudicated_by=$5
			WHERE id=$1`, disputeID, resolveStatus, award, note, actor(ctx).ID); err != nil {
			return err
		}
	} else {
		if _, err := tx.Exec(ctx, `
			UPDATE disputes SET status=$2, award_to_landlord=$3, resolution_note=$4, resolved_at=now()
			WHERE id=$1`, disputeID, resolveStatus, award, note); err != nil {
			return err
		}
	}
	if err := s.disputeAction(tx, ctx, disputeID, contractID, in.Action, award,
		in.EvidenceIDs, in.Comment); err != nil {
		return err
	}
	// 定论总费用拆分到分项:
	//   - 存在争议标记的品类: 按权重(已到账单优先, 否则估算)分摊裁决/协商金额 award;
	//   - 未涉争议的品类: 有账单按账单, 无账单按 0(平台可在 note 中说明)。
	var disputedKinds []string
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(array_agg(kind) FILTER (WHERE review_flag='SPLIT_DISPUTE'), '{}')
		FROM utility_reserve_items WHERE reservation_id=$1`, reservationID).Scan(&disputedKinds); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `
		SELECT kind, COALESCE(billed_amount, est_amount) AS w, billed_amount
		FROM utility_reserve_items WHERE reservation_id=$1 ORDER BY kind`, reservationID)
	if err != nil {
		return err
	}
	type kw struct {
		kind   string
		weight float64
		billed *float64
	}
	var kws []kw
	for rows.Next() {
		var k kw
		if err := rows.Scan(&k.kind, &k.weight, &k.billed); err != nil {
			rows.Close()
			return err
		}
		if k.weight < 0 {
			k.weight = 0
		}
		kws = append(kws, k)
	}
	rows.Close()
	isDisputed := func(k string) bool {
		for _, dk := range disputedKinds {
			if dk == k {
				return true
			}
		}
		return false
	}
	var disputeWeight float64
	for _, k := range kws {
		if isDisputed(k.kind) {
			disputeWeight += k.weight
		}
	}
	decision := UtilityReviewDecision{
		FinalAmounts: map[string]float64{}, Flags: map[string]string{},
		Blocker: "", Decision: "MANUAL",
	}
	allocated := 0.0
	disputedIdx := 0
	for _, k := range kws {
		var amt float64
		switch {
		case isDisputed(k.kind):
			disputedIdx++
			if disputeWeight <= 0 {
				// 争议品类无金额权重时平分裁决额, 末项吸收舍入差。
				amt = award / float64(len(disputedKinds))
			} else if disputedIdx == len(disputedKinds) {
				amt = round2(award - allocated)
			} else {
				amt = round2(award * k.weight / disputeWeight)
				allocated += amt
			}
		case k.billed != nil:
			amt = *k.billed // 未涉争议品类按其供应商账单正常结算
		default:
			amt = 0
		}
		decision.FinalAmounts[k.kind] = round2(amt)
		decision.Flags[k.kind] = domain.UtilityFlagOK
	}
	if len(disputedKinds) == 0 {
		// 未标记具体品类(如直接协商): 按所有权重分摊总金额。
		var totalW, alloc float64
		for _, k := range kws {
			totalW += k.weight
		}
		for i, k := range kws {
			amt := award
			if totalW > 0 {
				if i == len(kws)-1 {
					amt = round2(award - alloc)
				} else {
					amt = round2(award * k.weight / totalW)
					alloc += amt
				}
			} else if i != len(kws)-1 {
				amt = 0
			}
			decision.FinalAmounts[k.kind] = amt
		}
	}
	// 最终总额 = 争议品类定论额 + 其他已到账单品类的实际额(保证台账与分项一致)。
	total := 0.0
	for _, amt := range decision.FinalAmounts {
		total += amt
	}
	decision.FinalTotal = round2(total)
	if err := s.emit(tx, ctx, contractID, "DisputeResolved", map[string]any{
		"dispute_id": disputeID, "reservation_id": reservationID,
		"resolution": resolveStatus, "award_to_landlord": award,
	}, actor(ctx).ID); err != nil {
		return err
	}
	return s.finalizeUtilityReserve(tx, ctx, contractID, reservationID, decision, "MANUAL", note)
}

// ---------- 复核与最终结算(事务内) ----------

// reviewReserve 汇总各品类账单/照片读数, 调用纯函数复核。
func (s *Service) reviewReserve(tx pgx.Tx, ctx context.Context,
	contractID, reservationID string) (UtilityReviewDecision, error) {
	rows, err := tx.Query(ctx, `
		SELECT ri.kind, ci.reading, ri.last_reading, ri.unit_price,
		       ri.billed_reading, ri.billed_amount, ri.photo_reading, ri.photo_amount,
		       EXISTS(SELECT 1 FROM disputes d WHERE d.utility_reserve_id=ri.reservation_id
		              AND d.status IN ('OPEN','EVIDENCE_PENDING','UNDER_REVIEW'))
		FROM utility_reserve_items ri
		JOIN utility_readings ci ON ci.contract_id=$1 AND ci.phase='CHECKIN' AND ci.kind=ri.kind
		WHERE ri.reservation_id=$2 ORDER BY ri.kind`, contractID, reservationID)
	if err != nil {
		return UtilityReviewDecision{}, err
	}
	defer rows.Close()
	var items []UtilityReviewItem
	for rows.Next() {
		var kind string
		var checkin, last, price float64
		var billedReading, billedAmt, photoReading, photoAmt *float64
		var split bool
		if err := rows.Scan(&kind, &checkin, &last, &price,
			&billedReading, &billedAmt, &photoReading, &photoAmt, &split); err != nil {
			return UtilityReviewDecision{}, err
		}
		it := UtilityReviewItem{
			Kind: kind, CheckinReading: checkin, LastReading: last,
			Tolerance: readingTolerance, SplitDisputed: split,
		}
		if billedReading != nil {
			it.HasBill, it.BillReading, it.BilledAmount = true, *billedReading, *billedAmt
		}
		if photoReading != nil {
			it.HasPhoto, it.PhotoReading, it.PhotoAmount = true, *photoReading, *photoAmt
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return UtilityReviewDecision{}, err
	}
	return ReviewUtilityItems(items), nil
}

// finalizeUtilityReserve 执行唯一一次最终结算: utility_hold -> hard_hold(实际费用) + 多退/记录少补。
func (s *Service) finalizeUtilityReserve(tx pgx.Tx, ctx context.Context, contractID, reservationID string,
	d UtilityReviewDecision, decisionOverride, note string) error {
	// DB 唯一索引 uq_fund_ledger_one_utility_final 兜底; 这里再做一次显式检查给出友好错误。
	var already int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM fund_ledger WHERE contract_id=$1 AND txn_type='UTILITY_FINAL'`,
		contractID).Scan(&already); err != nil {
		return err
	}
	if already > 0 {
		return fmt.Errorf("%w: 最终水电押金流水已存在, 禁止重复扣款", ErrConflict)
	}

	var reserved float64
	if err := tx.QueryRow(ctx,
		`SELECT reserve_amount FROM utility_reservations WHERE id=$1 FOR UPDATE`,
		reservationID).Scan(&reserved); err != nil {
		return err
	}
	final := round2(d.FinalTotal)
	refund, shortage := 0.0, 0.0
	if final <= reserved {
		refund = round2(reserved - final)
	} else {
		shortage = round2(final - reserved)
	}

	// 持久化分项复核结论。
	rows, err := tx.Query(ctx,
		`SELECT kind FROM utility_reserve_items WHERE reservation_id=$1 ORDER BY kind`, reservationID)
	if err != nil {
		return err
	}
	var kinds []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			rows.Close()
			return err
		}
		kinds = append(kinds, k)
	}
	rows.Close()
	for _, k := range kinds {
		amt := d.FinalAmounts[k]
		flag := d.Flags[k]
		if flag == "" {
			flag = domain.UtilityFlagOK
		}
		var br, pr, lr *float64
		if err := tx.QueryRow(ctx, `
			SELECT billed_reading, photo_reading, last_reading FROM utility_reserve_items
			WHERE reservation_id=$1 AND kind=$2`, reservationID, k).Scan(&br, &pr, &lr); err != nil {
			return err
		}
		finalReading := lr
		switch {
		case pr != nil && (br == nil || absFloat(*pr-*br) <= readingTolerance):
			finalReading = pr
		case br != nil:
			finalReading = br
		}
		if _, err := tx.Exec(ctx, `
			UPDATE utility_reserve_items
			SET final_reading=$2, final_amount=$3, review_flag=$4
			WHERE reservation_id=$1 AND kind=$5`, reservationID, finalReading, amt, flag, k); err != nil {
			return err
		}
	}

	decision := d.Decision
	if decisionOverride != "" {
		decision = decisionOverride
	}
	if note == "" {
		note = fmt.Sprintf("账单合计 %.2f, 照片口径 %.2f, 定论 %.2f; 预留 %.2f, 退回 %.2f, 补缴 %.2f",
			d.BilledTotal, d.PhotoTotal, final, reserved, refund, shortage)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE utility_reservations
		SET status='FINALIZED', billed_total=$2, photo_total=$3, final_amount=$4, final_decision=$5,
		    review_note=$6, refund_amount=$7, shortage_amount=$8,
		    reviewed_at=now(), finalized_at=now(), finalized_by=$9
		WHERE id=$1`, reservationID, d.BilledTotal, d.PhotoTotal, final, decision, note,
		refund, shortage, actor(ctx).ID); err != nil {
		return err
	}

	// 唯一一笔最终押金流水: 实际费用 utility_hold -> hard_hold; 多出部分随 utility_hold 释放回可退余额。
	if err := s.ledger(tx, ctx, contractID, "UTILITY_FINAL", final,
		0, final, 0, -reserved,
		"utility_reserve", reservationID, note, "utility-final:"+contractID); err != nil {
		return err
	}

	c, _ := s.getContract(tx, ctx, contractID)
	_ = s.notify(tx, ctx, contractID, c.TenantID, "utility_finalized",
		map[string]any{"final": final, "refund": refund, "shortage": shortage})
	_ = s.notify(tx, ctx, contractID, c.LandlordID, "utility_finalized",
		map[string]any{"final": final, "refund": refund, "shortage": shortage})
	return s.emit(tx, ctx, contractID, "UtilityReserveFinalized", map[string]any{
		"reservation_id": reservationID, "billed_total": d.BilledTotal,
		"photo_total": d.PhotoTotal, "final_amount": final, "decision": decision,
		"reserve_amount": reserved, "refund_amount": refund, "shortage_amount": shortage,
	}, actor(ctx).ID)
}

// recomputeReserveAfterDispute 争议闭合后重估预留状态: 数据齐备则自动结算, 否则回到待处理/人工复核。
func (s *Service) recomputeReserveAfterDispute(tx pgx.Tx, ctx context.Context,
	contractID, reservationID string) error {
	var st string
	if err := tx.QueryRow(ctx, `SELECT status FROM utility_reservations WHERE id=$1`,
		reservationID).Scan(&st); err != nil {
		return err
	}
	if st == domain.UtilityReserveFinalized {
		return nil
	}
	var openN int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM disputes WHERE utility_reserve_id=$1
		  AND status IN ('OPEN','EVIDENCE_PENDING','UNDER_REVIEW')`, reservationID).Scan(&openN); err != nil {
		return err
	}
	if openN > 0 {
		return nil
	}
	// 争议已闭合, 清除分项上残留的争议标记(自动复核依据的是 disputes 表实时状态)。
	if _, err := tx.Exec(ctx, `
		UPDATE utility_reserve_items SET review_flag='OK'
		WHERE reservation_id=$1 AND review_flag='SPLIT_DISPUTE'`, reservationID); err != nil {
		return err
	}
	d, err := s.reviewReserve(tx, ctx, contractID, reservationID)
	if err != nil {
		return err
	}
	if d.Blocker == "" {
		return s.finalizeUtilityReserve(tx, ctx, contractID, reservationID, d, "",
			"分摊争议闭合, 账单与搬离读数复核一致, 自动最终结算")
	}
	newSt := domain.UtilityReserveReviewPending
	if d.Blocker == domain.UtilityFlagReadingMismatch {
		newSt = domain.UtilityReserveManualReview
	}
	_, err = tx.Exec(ctx, `UPDATE utility_reservations SET status=$2 WHERE id=$1`, reservationID, newSt)
	return err
}

func (s *Service) reserveStatus(tx pgx.Tx, ctx context.Context, id string) (string, error) {
	var st string
	err := tx.QueryRow(ctx, `SELECT status FROM utility_reservations WHERE id=$1`, id).Scan(&st)
	return st, err
}

func (s *Service) reserveMoneyOutcome(tx pgx.Tx, ctx context.Context, id string) (refund, shortage, final float64, err error) {
	err = tx.QueryRow(ctx,
		`SELECT refund_amount, shortage_amount, final_amount FROM utility_reservations WHERE id=$1`,
		id).Scan(&refund, &shortage, &final)
	return
}
