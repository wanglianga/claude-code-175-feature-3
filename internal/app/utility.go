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

// 退租水电尾款结算。
//
// 租客搬离时最后一期水电煤账单尚未生成, 平台先依据最近读数与历史用量从押金中预留一笔
// (utility_reservations, 资金入 hard_hold, 台账 UTILITY_RESERVE), 向双方展示预留原因与
// 预计结算时间。供应商账单到达后登记(utility_bills, 账单号幂等); 租客补传搬离当天的
// 水表/电表照片(meter_photos)时, 平台必须把照片读数与账单逐项复核(meter_reviews)后,
// 才生成唯一一笔最终押金流水(final_utility_settlements, 台账 UTILITY_FINAL_SETTLE):
// 多出的预留退还租客, 不足部分形成补缴(utility_supplements)。账单延迟、读数无法对应或
// 分摊有争议时, 预留保持 MANUAL_REVIEW, 不得最终结算。
//
// 所有资金动作与 0001 主流程一样: 同一合同 advisory xact lock 事务内联动 escrow 三层金额。

const (
	UtilityReserveStatusReserved      = "RESERVED"
	UtilityReserveStatusBillReceived  = "BILL_RECEIVED"
	UtilityReserveStatusReviewPending = "REVIEW_PENDING"
	UtilityReserveStatusManualReview  = "MANUAL_REVIEW"
	UtilityReserveStatusSettled       = "SETTLED"

	UtilityManualBillDelayed        = "BILL_DELAYED"
	UtilityManualReadingUnmatchable = "READING_UNMATCHABLE"
	UtilityManualAllocationDispute  = "ALLOCATION_DISPUTE"
)

// ---------- 输入类型 ----------

type CreateUtilityReservationInput struct {
	// ReservedAmount=0 时按最近读数与历史用量自动估算。
	ReservedAmount     float64 `json:"reserved_amount"`
	Reason             string  `json:"reason"`
	ExpectedSettleDays int     `json:"expected_settle_days"` // 预计结算时间(天数, 默认 14)
	BillDueDays        int     `json:"bill_due_days"`        // 供应商预计出账时间(天数, 默认同预计结算)
	Note               string  `json:"note"`
}

type UtilityBillLine struct {
	Kind         string  `json:"kind"`
	StartReading float64 `json:"start_reading"`
	EndReading   float64 `json:"end_reading"`
	Usage        float64 `json:"usage"`
	UnitPrice    float64 `json:"unit_price"`
	Amount       float64 `json:"amount"`
}

type SubmitSupplierBillInput struct {
	BillNo          string            `json:"bill_no"` // 幂等键: 同一合同同一账单号不重复扣款
	Supplier        string            `json:"supplier"`
	BillPeriodStart string            `json:"bill_period_start"`
	BillPeriodEnd   string            `json:"bill_period_end"`
	LineItems       []UtilityBillLine `json:"line_items"`
	TotalAmount     float64           `json:"total_amount"`
	EvidenceIDs     []string          `json:"evidence_ids"`
	Note            string            `json:"note"`
}

type MeterPhotoInput struct {
	Kind       string  `json:"kind"` // WATER/ELECTRIC/GAS
	Reading    float64 `json:"reading"`
	EvidenceID string  `json:"evidence_id"`
	TakenAt    string  `json:"taken_at"` // RFC3339, 搬离当天
	Note       string  `json:"note"`
}

type SubmitMeterPhotosInput struct {
	Photos []MeterPhotoInput `json:"photos"`
	Note   string            `json:"note"`
}

type ReviewMeterInput struct {
	// MATCH 照片与账单读数一致, 按账单金额; ADOPT_PHOTO 采用照片读数重算;
	// UNMATCHABLE 读数无法对应, 转人工复核。
	Decision  string             `json:"decision"`
	Overrides map[string]float64 `json:"amount_overrides"` // 可选: 平台逐项拍板金额(kind->amount)
	Note      string             `json:"note"`
}

type ResolveUtilityManualInput struct {
	// RESUME 人工复核结束, 恢复到可结算(BILL_RECEIVED); 账单延迟且供应商始终不出账时,
	// 平台可传 SettledAmount 形成核定金额(无账单时自动生成一张平台核定单)。
	Action        string  `json:"action"`
	SettledAmount float64 `json:"settled_amount"`
	Note          string  `json:"note"`
}

// ---------- 1. 创建预留 ----------

// CreateUtilityReservation 在租客搬离、最后一期账单未出时预留部分押金。
func (s *Service) CreateUtilityReservation(ctx context.Context, contractID string,
	in CreateUtilityReservationInput) (string, error) {
	var rid string
	err := s.runTx(ctx, contractID, func(tx pgx.Tx) error {
		if _, err := s.assertParty(tx, ctx, contractID, "LANDLORD", "AGENT", "PLATFORM"); err != nil {
			return err
		}
		var st string
		if err := tx.QueryRow(ctx, `SELECT status FROM contracts WHERE id=$1`, contractID).Scan(&st); err != nil {
			return err
		}
		if err := checkStatus(st,
			domain.StatusCheckoutPending, domain.StatusEarlyTerminated, domain.StatusInspecting,
			domain.StatusDeductionProposed, domain.StatusDisputing); err != nil {
			return err
		}
		var exists bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM utility_reservations WHERE contract_id=$1)`, contractID).
			Scan(&exists); err != nil {
			return err
		}
		if exists {
			return fmt.Errorf("%w: 水电尾款预留已存在, 一份合同至多一笔", ErrConflict)
		}

		breakdown, estTotal, basisLines, err := s.estimateUtility(tx, ctx, contractID)
		if err != nil {
			return err
		}
		reserved := round2(in.ReservedAmount)
		if reserved <= 0 {
			reserved = estTotal
		}
		if reserved <= 0 {
			return fmt.Errorf("%w: 无法自动估算预留金额, 请显式提供 reserved_amount", ErrValidation)
		}
		// 预留只能占用"未被任何建议/争议占用"的可退余额。
		var balance, hard, disputed float64
		if err := tx.QueryRow(ctx,
			`SELECT balance, hard_hold, disputed_hold FROM escrow_accounts WHERE contract_id=$1`,
			contractID).Scan(&balance, &hard, &disputed); err != nil {
			return fmt.Errorf("escrow: %w", err)
		}
		if available := round2(balance - hard - disputed); reserved > available+0.001 {
			return fmt.Errorf("%w: 预留 %.2f 超过可占用押金余额 %.2f",
				ErrAmountExceeds, reserved, available)
		}

		if in.ExpectedSettleDays <= 0 {
			in.ExpectedSettleDays = 14
		}
		// bill_due_days=0 默认与预计结算一致; 允许显式负值(表示出账时间已过, 供延迟巡检/测试)。
		if in.BillDueDays == 0 {
			in.BillDueDays = in.ExpectedSettleDays
		}
		expectedAt := nowUTC().AddDate(0, 0, in.ExpectedSettleDays)
		billDueAt := nowUTC().AddDate(0, 0, in.BillDueDays)
		reason := in.Reason
		if reason == "" {
			reason = "租客已搬离, 最后一期水电煤供应商账单尚未生成; 依据最近表计读数与历史用量预留, " +
				"账单到达后按实际费用多退少补"
		}

		b, _ := json.Marshal(breakdown)
		if err := tx.QueryRow(ctx, `
			INSERT INTO utility_reservations(contract_id,status,reserved_amount,estimate_breakdown,
			                                 reason,expected_settle_at,bill_due_at,note,created_by)
			VALUES($1,'RESERVED',$2,$3,$4,$5,$6,$7,$8) RETURNING id::text`,
			contractID, reserved, string(b), reason, expectedAt, billDueAt, in.Note,
			actor(ctx).ID).Scan(&rid); err != nil {
			return fmt.Errorf("create reservation: %w", err)
		}
		// 预留进入 hard_hold: 这笔钱在最终结算前双方都拿不到。
		if err := s.ledger(tx, ctx, contractID, "UTILITY_RESERVE", reserved, 0, reserved, 0,
			"utility_reservation", rid, "退租水电尾款预留(最后一期账单未出): "+reason,
			"utility-reserve:"+contractID); err != nil {
			return err
		}
		c, _ := s.getContract(tx, ctx, contractID)
		_ = s.notify(tx, ctx, contractID, c.TenantID, "utility_reserved", map[string]any{
			"amount": reserved, "reason": reason,
			"expected_settle_at": expectedAt.Format(time.RFC3339), "basis": basisLines,
		})
		_ = s.notify(tx, ctx, contractID, c.LandlordID, "utility_reserved", map[string]any{
			"amount": reserved, "expected_settle_at": expectedAt.Format(time.RFC3339),
		})
		return s.emit(tx, ctx, contractID, "UtilityReservationCreated", map[string]any{
			"reservation_id": rid, "reserved_amount": reserved, "reason": reason,
			"expected_settle_at": expectedAt, "estimate": breakdown,
		}, actor(ctx).ID)
	})
	return rid, err
}

// estimateLine 是估算口径(同时存进 estimate_breakdown 供双方查看)。
type estimateLine struct {
	Kind        string  `json:"kind"`
	LastReading float64 `json:"last_reading"`
	MonthlyAvg  float64 `json:"monthly_avg"`
	EstAmount   float64 `json:"est_amount"`
	UnitPrice   float64 `json:"unit_price"`
	Basis       string  `json:"basis"`
}

// estimateUtility 按 入住->退租区间账单 / 租期月数 得月均, 预留 2 个月用量(封顶不超押金由调用方校验)。
func (s *Service) estimateUtility(tx pgx.Tx, ctx context.Context, contractID string) ([]estimateLine, float64, []string, error) {
	rows, err := tx.Query(ctx, `
		SELECT co.kind, co.reading,
		       COALESCE(NULLIF(co.unit_price,0), ci.unit_price),
		       co.billed_amount,
		       GREATEST(EXTRACT(EPOCH FROM (co.recorded_at - ci.recorded_at))/86400.0/30.0, 1)
		FROM utility_readings co
		JOIN utility_readings ci ON ci.contract_id=co.contract_id AND ci.kind=co.kind AND ci.phase='CHECKIN'
		WHERE co.contract_id=$1 AND co.phase='CHECKOUT'`, contractID)
	if err != nil {
		return nil, 0, nil, err
	}
	defer rows.Close()
	var lines []estimateLine
	var basis []string
	total := 0.0
	kindCN := map[string]string{"WATER": "水", "ELECTRIC": "电", "GAS": "燃气"}
	for rows.Next() {
		var kind string
		var last, price, billed, months float64
		if err := rows.Scan(&kind, &last, &price, &billed, &months); err != nil {
			return nil, 0, nil, err
		}
		monthly := round2(billed / months)
		est := round2(monthly * 2) // 预留 2 个月的历史月均, 覆盖延迟出账
		l := estimateLine{
			Kind: kind, LastReading: round2(last), MonthlyAvg: monthly, EstAmount: est,
			UnitPrice: price,
			Basis: fmt.Sprintf("最近%s表读数 %.2f, 租期月均 %.2f 元, 按 2 个月预留 %.2f 元",
				kindCN[kind], last, monthly, est),
		}
		lines = append(lines, l)
		basis = append(basis, l.Basis)
		total += est
	}
	return lines, round2(total), basis, rows.Err()
}

// ---------- 2. 登记供应商账单(账单号幂等) ----------

// SubmitSupplierBill 登记最后一期供应商账单。重复收到同一 bill_no 直接返回原记录, 绝不重复扣款。
func (s *Service) SubmitSupplierBill(ctx context.Context, contractID, reservationID string,
	in SubmitSupplierBillInput) (billID string, duplicated bool, err error) {
	err = s.runTx(ctx, contractID, func(tx pgx.Tx) error {
		if _, err := s.assertParty(tx, ctx, contractID, "LANDLORD", "AGENT", "PLATFORM"); err != nil {
			return err
		}
		if in.BillNo == "" {
			return fmt.Errorf("%w: bill_no 必填(账单号用于幂等去重)", ErrValidation)
		}
		var rst string
		if err := tx.QueryRow(ctx,
			`SELECT status FROM utility_reservations WHERE id=$1 AND contract_id=$2`,
			reservationID, contractID).Scan(&rst); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		// 幂等: 同一账单号重复上传, 原样返回, 不改变状态、不产生任何资金动作。
		var existingStatus string
		eerr := tx.QueryRow(ctx,
			`SELECT id::text, status FROM utility_bills WHERE contract_id=$1 AND bill_no=$2`,
			contractID, in.BillNo).Scan(&billID, &existingStatus)
		if eerr == nil {
			duplicated = true
			return nil
		}
		if !errors.Is(eerr, pgx.ErrNoRows) {
			return eerr
		}
		if rst == UtilityReserveStatusSettled {
			return fmt.Errorf("%w: 预留已最终结算, 新账单不能再次扣款, 请走客服争议渠道", ErrBadState)
		}
		// 同一预留只允许一份在途(RECEIVED)账单, 防止拿两张不同账单重复主张。
		var n int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM utility_bills WHERE reservation_id=$1 AND status='RECEIVED'`,
			reservationID).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return fmt.Errorf("%w: 已有在途账单, 请先完成照片复核或由平台作废后再登记", ErrConflict)
		}

		total := round2(in.TotalAmount)
		if total <= 0 && len(in.LineItems) > 0 {
			for _, l := range in.LineItems {
				total += l.Amount
			}
			total = round2(total)
		}
		lb, _ := json.Marshal(in.LineItems)
		supplier := in.Supplier
		if supplier == "" {
			supplier = "水电煤供应商"
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO utility_bills(contract_id,reservation_id,bill_no,supplier,
			                          bill_period_start,bill_period_end,line_items,total_amount,
			                          evidence_ids,note,uploaded_by)
			VALUES($1,$2,$3,$4,NULLIF($5,'')::date,NULLIF($6,'')::date,$7,$8,$9::uuid[],$10,$11)
			RETURNING id::text`,
			contractID, reservationID, in.BillNo, supplier,
			in.BillPeriodStart, in.BillPeriodEnd, string(lb), total,
			pgUUIDArrayLiteral(in.EvidenceIDs), in.Note, actor(ctx).ID).Scan(&billID); err != nil {
			return fmt.Errorf("register bill: %w", err)
		}

		// 账单到达后的预留状态:
		//  - 租客已补传过搬离当天读数照片 -> 必须先复核(REVIEW_PENDING), 不能只按账单金额扣款;
		//  - 账单延迟(BILL_DELAYED)的人工复核, 账单到达自动回到正常流程;
		//  - 读数无法对应/分摊争议的人工复核不因新账单自动解除(争议未闭合或未复核前保持待处理)。
		var photoN int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM meter_photos WHERE reservation_id=$1 AND superseded=false`,
			reservationID).Scan(&photoN); err != nil {
			return err
		}
		newSt := UtilityReserveStatusBillReceived
		if photoN > 0 {
			newSt = UtilityReserveStatusReviewPending
		}
		keepManual := false
		if rst == UtilityReserveStatusManualReview {
			var reason *string
			if err := tx.QueryRow(ctx,
				`SELECT manual_reason FROM utility_reservations WHERE id=$1`,
				reservationID).Scan(&reason); err != nil {
				return err
			}
			if reason == nil || *reason != UtilityManualBillDelayed {
				keepManual = true
			}
			// 账单延迟但已存在水电尾款争议的, 也保持人工态
			var dn int
			if err := tx.QueryRow(ctx, `
				SELECT count(*) FROM disputes WHERE contract_id=$1 AND reason='UTILITY_FINAL_SETTLEMENT'
				  AND status IN ('OPEN','EVIDENCE_PENDING','UNDER_REVIEW')`, contractID).Scan(&dn); err != nil {
				return err
			}
			if dn > 0 {
				keepManual = true
			}
		}
		if keepManual {
			c, _ := s.getContract(tx, ctx, contractID)
			_ = s.notify(tx, ctx, contractID, c.TenantID, "utility_bill_received", map[string]any{
				"bill_no": in.BillNo, "total": total, "manual_review_kept": true,
			})
			return s.emit(tx, ctx, contractID, "UtilitySupplierBillReceived", map[string]any{
				"reservation_id": reservationID, "bill_id": billID, "bill_no": in.BillNo,
				"total_amount": total, "duplicated": false, "need_photo_review": photoN > 0,
				"manual_review_kept": true,
			}, actor(ctx).ID)
		}
		return s.transitionReservation(tx, ctx, contractID, reservationID, newSt,
			map[string]any{
				"manual_reason": nil, "bill_overdue": false,
			}, true, func() error {
				c, _ := s.getContract(tx, ctx, contractID)
				_ = s.notify(tx, ctx, contractID, c.TenantID, "utility_bill_received", map[string]any{
					"bill_no": in.BillNo, "total": total,
					"need_photo_review": photoN > 0,
				})
				return s.emit(tx, ctx, contractID, "UtilitySupplierBillReceived", map[string]any{
					"reservation_id": reservationID, "bill_id": billID, "bill_no": in.BillNo,
					"total_amount": total, "duplicated": false, "need_photo_review": photoN > 0,
				}, actor(ctx).ID)
			})
	})
	return billID, duplicated, err
}

// ---------- 3. 租客补传搬离当天读表照片 ----------

// SubmitMeterPhotos 租客补传搬离当天水表/电表照片读数。重复补传只替换为最新版本(旧读数留痕),
// 在最终结算之前任何时候都不会触发扣款。
func (s *Service) SubmitMeterPhotos(ctx context.Context, contractID, reservationID string,
	in SubmitMeterPhotosInput) error {
	return s.runTx(ctx, contractID, func(tx pgx.Tx) error {
		if _, err := s.assertParty(tx, ctx, contractID, "TENANT", "PLATFORM"); err != nil {
			return err
		}
		if len(in.Photos) == 0 {
			return fmt.Errorf("%w: photos 不能为空", ErrValidation)
		}
		var rst string
		if err := tx.QueryRow(ctx,
			`SELECT status FROM utility_reservations WHERE id=$1 AND contract_id=$2`,
			reservationID, contractID).Scan(&rst); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if rst == UtilityReserveStatusSettled {
			// 已最终结算: 接受材料但绝不重复扣款, 引导走争议/客服。
			return fmt.Errorf("%w: 水电尾款已最终结算, 补传照片不会重复扣款; 如有异议请提起分摊争议",
				ErrBadState)
		}
		var added int
		for _, p := range in.Photos {
			switch p.Kind {
			case "WATER", "ELECTRIC", "GAS":
			default:
				return fmt.Errorf("%w: kind 必须 WATER/ELECTRIC/GAS", ErrValidation)
			}
			if p.EvidenceID == "" {
				return fmt.Errorf("%w: %s 照片 evidence_id 必填", ErrValidation, p.Kind)
			}
			// 证据必须属于本合同且是水电类(防止拿别的照片冒认)。
			var ok bool
			if err := tx.QueryRow(ctx, `
				SELECT EXISTS(SELECT 1 FROM evidence
				 WHERE id=$1::uuid AND contract_id=$2 AND category='UTILITY')`,
				p.EvidenceID, contractID).Scan(&ok); err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("%w: evidence %s 不是本合同的 UTILITY 类证据", ErrValidation, p.EvidenceID)
			}
			// 幂等: 同类表读数与证据都相同 -> 忽略。
			var same bool
			if err := tx.QueryRow(ctx, `
				SELECT EXISTS(SELECT 1 FROM meter_photos
				 WHERE reservation_id=$1 AND kind=$2 AND reading=$3
				   AND evidence_id=$4::uuid AND superseded=false)`,
				reservationID, p.Kind, p.Reading, p.EvidenceID).Scan(&same); err != nil {
				return err
			}
			if same {
				continue
			}
			takenAt := nowUTC()
			if p.TakenAt != "" {
				t, perr := time.Parse(time.RFC3339, p.TakenAt)
				if perr != nil {
					return fmt.Errorf("%w: taken_at 需要 RFC3339 时间", ErrValidation)
				}
				takenAt = t.UTC()
			}
			// 旧有效读数标记 superseded(部分唯一索引保证每类仅一条 active)。
			if _, err := tx.Exec(ctx, `
				UPDATE meter_photos SET superseded=true
				WHERE reservation_id=$1 AND kind=$2 AND superseded=false`,
				reservationID, p.Kind); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO meter_photos(contract_id,reservation_id,kind,reading,evidence_id,
				                          taken_at,note,uploaded_by)
				VALUES($1,$2,$3,$4,$5::uuid,$6,$7,$8)`,
				contractID, reservationID, p.Kind, p.Reading, p.EvidenceID,
				takenAt, p.Note, actor(ctx).ID); err != nil {
				return fmt.Errorf("insert meter photo: %w", err)
			}
			added++
		}
		if added == 0 {
			return nil // 全部为重复提交, 幂等成功
		}
		// 账单已在 -> 进入待复核(必须照片与账单一起核对, 不能只按账单扣);
		// 账单未到 -> 维持 RESERVED, 账单到达时会自动转 REVIEW_PENDING。
		var billN int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM utility_bills WHERE reservation_id=$1 AND status='RECEIVED'`,
			reservationID).Scan(&billN); err != nil {
			return err
		}
		if billN > 0 && rst != UtilityReserveStatusManualReview {
			if err := s.updateReservationStatus(tx, ctx, reservationID,
				UtilityReserveStatusReviewPending, nil); err != nil {
				return err
			}
		}
		c, _ := s.getContract(tx, ctx, contractID)
		_ = s.notify(tx, ctx, contractID, c.LandlordID, "utility_meter_photos", map[string]any{
			"count": added, "note": in.Note,
		})
		return s.emit(tx, ctx, contractID, "UtilityMeterPhotosSubmitted", map[string]any{
			"reservation_id": reservationID, "added": added, "bill_exists": billN > 0,
		}, actor(ctx).ID)
	})
}

// ---------- 4. 平台复核: 照片读数 vs 供应商账单 ----------

type meterLineResult struct {
	Kind           string  `json:"kind"`
	BillEndReading float64 `json:"bill_end_reading"`
	PhotoReading   float64 `json:"photo_reading"`
	AdoptedReading float64 `json:"adopted_reading"`
	UnitPrice      float64 `json:"unit_price"`
	BillAmount     float64 `json:"bill_amount"`
	AdoptedAmount  float64 `json:"adopted_amount"`
	Result         string  `json:"result"` // MATCH/PHOTO_ADOPTED/NO_PHOTO/UNMATCHABLE
}

// ReviewMeterEvidence 平台把租客照片读数与供应商账单放在一起逐项复核。
func (s *Service) ReviewMeterEvidence(ctx context.Context, contractID, reservationID string,
	in ReviewMeterInput) (string, error) {
	var reviewID string
	err := s.runTx(ctx, contractID, func(tx pgx.Tx) error {
		if actor(ctx).Role != "PLATFORM" {
			return fmt.Errorf("%w: 仅平台可执行照片/账单复核", ErrForbidden)
		}
		switch in.Decision {
		case "MATCH", "ADOPT_PHOTO", "UNMATCHABLE":
		default:
			return fmt.Errorf("%w: decision 必须 MATCH/ADOPT_PHOTO/UNMATCHABLE", ErrValidation)
		}
		var rst string
		if err := tx.QueryRow(ctx,
			`SELECT status FROM utility_reservations WHERE id=$1 AND contract_id=$2`,
			reservationID, contractID).Scan(&rst); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if rst == UtilityReserveStatusSettled {
			return fmt.Errorf("%w: 已最终结算, 不能再复核", ErrBadState)
		}
		var billID string
		var lineItemsJSON []byte
		var billTotal float64
		if err := tx.QueryRow(ctx, `
			SELECT id::text, line_items, total_amount FROM utility_bills
			WHERE reservation_id=$1 AND status='RECEIVED'`,
			reservationID).Scan(&billID, &lineItemsJSON, &billTotal); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: 供应商账单尚未到达, 无法复核", ErrBadState)
			}
			return err
		}
		var photoN int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM meter_photos WHERE reservation_id=$1 AND superseded=false`,
			reservationID).Scan(&photoN); err != nil {
			return err
		}
		if photoN == 0 {
			return fmt.Errorf("%w: 租客未补传读表照片, 无需交叉复核(可直接按账单最终结算)", ErrBadState)
		}

		if in.Decision == "UNMATCHABLE" {
			if err := tx.QueryRow(ctx, `
				INSERT INTO meter_reviews(contract_id,reservation_id,bill_id,decision,note,reviewed_by)
				VALUES($1,$2,$3,'UNMATCHABLE',$4,$5) RETURNING id::text`,
				contractID, reservationID, billID, in.Note, actor(ctx).ID).Scan(&reviewID); err != nil {
				return err
			}
			if err := s.updateReservationStatus(tx, ctx, reservationID,
				UtilityReserveStatusManualReview,
				map[string]any{"manual_reason": UtilityManualReadingUnmatchable}); err != nil {
				return err
			}
			c, _ := s.getContract(tx, ctx, contractID)
			_ = s.notify(tx, ctx, contractID, c.TenantID, "utility_manual_review",
				map[string]any{"reason": "READING_UNMATCHABLE", "note": in.Note})
			return s.emit(tx, ctx, contractID, "UtilityMeterEvidenceReviewed", map[string]any{
				"reservation_id": reservationID, "review_id": reviewID, "decision": "UNMATCHABLE",
			}, actor(ctx).ID)
		}

		// 逐项核对
		var billLines []UtilityBillLine
		if len(lineItemsJSON) > 0 {
			if err := json.Unmarshal(lineItemsJSON, &billLines); err != nil {
				return fmt.Errorf("bill line_items 解析失败: %w", err)
			}
		}
		results, adoptedTotal, err := s.crossCheckLines(tx, ctx, contractID, reservationID,
			billLines, billTotal, in)
		if err != nil {
			return err
		}
		rb, _ := json.Marshal(results)
		if err := tx.QueryRow(ctx, `
			INSERT INTO meter_reviews(contract_id,reservation_id,bill_id,decision,line_results,note,reviewed_by)
			VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id::text`,
			contractID, reservationID, billID, in.Decision, string(rb), in.Note,
			actor(ctx).ID).Scan(&reviewID); err != nil {
			return err
		}
		// 复核结论回写账单核定金额(不改账单原值, 保留审计痕迹)。
		if _, err := tx.Exec(ctx,
			`UPDATE utility_bills SET settled_amount=$2 WHERE id=$1`, billID, adoptedTotal); err != nil {
			return err
		}
		if err := s.updateReservationStatus(tx, ctx, reservationID,
			UtilityReserveStatusBillReceived, map[string]any{"manual_reason": nil}); err != nil {
			return err
		}
		c, _ := s.getContract(tx, ctx, contractID)
		_ = s.notify(tx, ctx, contractID, c.TenantID, "utility_review_done", map[string]any{
			"decision": in.Decision, "settled_amount": adoptedTotal,
		})
		_ = s.notify(tx, ctx, contractID, c.LandlordID, "utility_review_done", map[string]any{
			"decision": in.Decision, "settled_amount": adoptedTotal,
		})
		return s.emit(tx, ctx, contractID, "UtilityMeterEvidenceReviewed", map[string]any{
			"reservation_id": reservationID, "review_id": reviewID, "decision": in.Decision,
			"bill_total": billTotal, "settled_amount": adoptedTotal, "lines": results,
		}, actor(ctx).ID)
	})
	return reviewID, err
}

// crossCheckLines 把账单分项与照片读数逐项摆在一起, 得出复核口径与核定总额。
func (s *Service) crossCheckLines(tx pgx.Tx, ctx context.Context, contractID, reservationID string,
	billLines []UtilityBillLine, billTotal float64, in ReviewMeterInput) ([]meterLineResult, float64, error) {
	// 照片读数
	photos := map[string]float64{}
	prows, err := tx.Query(ctx, `
		SELECT kind, reading FROM meter_photos
		WHERE reservation_id=$1 AND superseded=false`, reservationID)
	if err != nil {
		return nil, 0, err
	}
	for prows.Next() {
		var k string
		var r float64
		if err := prows.Scan(&k, &r); err != nil {
			prows.Close()
			return nil, 0, err
		}
		photos[k] = r
	}
	prows.Close()
	if err := prows.Err(); err != nil {
		return nil, 0, err
	}
	// 入住起点读数(账单分项缺 start_reading 时兜底)
	checkin := map[string]float64{}
	crows, err := tx.Query(ctx, `
		SELECT kind, reading FROM utility_readings
		WHERE contract_id=$1 AND phase='CHECKIN'`, contractID)
	if err != nil {
		return nil, 0, err
	}
	for crows.Next() {
		var k string
		var r float64
		if err := crows.Scan(&k, &r); err != nil {
			crows.Close()
			return nil, 0, err
		}
		checkin[k] = r
	}
	crows.Close()
	if err := crows.Err(); err != nil {
		return nil, 0, err
	}

	results := []meterLineResult{}
	total := 0.0
	seen := map[string]bool{}
	for _, bl := range billLines {
		seen[bl.Kind] = true
		photo, hasPhoto := photos[bl.Kind]
		start := bl.StartReading
		if start == 0 {
			start = checkin[bl.Kind]
		}
		res := meterLineResult{
			Kind: bl.Kind, BillEndReading: round2(bl.EndReading), PhotoReading: round2(photo),
			UnitPrice: bl.UnitPrice, BillAmount: round2(bl.Amount),
		}
		switch {
		case in.Overrides[bl.Kind] > 0:
			// 平台逐项拍板优先
			res.AdoptedAmount = round2(in.Overrides[bl.Kind])
			res.AdoptedReading = photo
			res.Result = "PHOTO_ADOPTED"
		case in.Decision == "MATCH":
			res.AdoptedAmount = round2(bl.Amount)
			res.AdoptedReading = round2(bl.EndReading)
			if hasPhoto {
				res.Result = "MATCH"
			} else {
				res.Result = "NO_PHOTO"
			}
		case hasPhoto: // ADOPT_PHOTO: 以照片读数为准重算用量
			usage := photo - start
			if usage < 0 {
				usage = 0
			}
			res.AdoptedReading = round2(photo)
			res.AdoptedAmount = round2(usage * bl.UnitPrice)
			res.Result = "PHOTO_ADOPTED"
		default:
			// 该类表租客没传照片, 沿用账单金额(只复核有照片的种类)。
			res.AdoptedAmount = round2(bl.Amount)
			res.AdoptedReading = round2(bl.EndReading)
			res.Result = "NO_PHOTO"
		}
		results = append(results, res)
		total += res.AdoptedAmount
	}
	// 账单没有分项时, 只能整额核对: ADOPT_PHOTO 必须有平台 override, 否则按账单总额。
	if len(billLines) == 0 {
		amount := round2(billTotal)
		if in.Decision == "ADOPT_PHOTO" {
			if len(in.Overrides) == 0 {
				return nil, 0, fmt.Errorf("%w: 账单无分项, ADOPT_PHOTO 需平台在 amount_overrides 给出核定金额",
					ErrValidation)
			}
			amount = 0
			for _, v := range in.Overrides {
				amount += v
			}
			amount = round2(amount)
		}
		for kind, photo := range photos {
			results = append(results, meterLineResult{
				Kind: kind, PhotoReading: round2(photo), AdoptedReading: round2(photo),
				BillAmount: round2(billTotal), AdoptedAmount: amount,
				Result: map[bool]string{true: "MATCH", false: "PHOTO_ADOPTED"}[in.Decision == "MATCH"],
			})
		}
		total = amount
	}
	return results, round2(total), nil
}

// ---------- 5. 人工复核: 账单延迟 / 分摊争议 ----------

// MarkUtilityBillDelayed 平台(巡检)把超过预计出账时间仍未收到账单的预留标记为账单延迟,
// 预留款保持待处理。
func (s *Service) MarkUtilityBillDelayed(ctx context.Context) ([]string, error) {
	if actor(ctx).Role != "PLATFORM" {
		return nil, fmt.Errorf("%w: platform only", ErrForbidden)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, contract_id::text FROM utility_reservations
		WHERE status='RESERVED' AND bill_due_at < now() AND bill_overdue=false`)
	if err != nil {
		return nil, err
	}
	type pair struct{ rid, cid string }
	var list []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.rid, &p.cid); err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, p)
	}
	rows.Close()
	var out []string
	for _, p := range list {
		err := s.runTx(ctx, p.cid, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `
				UPDATE utility_reservations SET status='MANUAL_REVIEW', bill_overdue=true,
				                                 manual_reason='BILL_DELAYED', status_updated_at=now()
				WHERE id=$1 AND status='RESERVED'`, p.rid); err != nil {
				return err
			}
			if err := s.emit(tx, ctx, p.cid, "UtilityBillDelayed", map[string]any{
				"reservation_id": p.rid,
			}, actor(ctx).ID); err != nil {
				return err
			}
			c, _ := s.getContract(tx, ctx, p.cid)
			_ = s.notify(tx, ctx, p.cid, c.TenantID, "utility_manual_review",
				map[string]any{"reason": "BILL_DELAYED"})
			_ = s.notify(tx, ctx, p.cid, c.LandlordID, "utility_manual_review",
				map[string]any{"reason": "BILL_DELAYED"})
			return nil
		})
		if err != nil {
			return out, err
		}
		out = append(out, p.rid)
	}
	return out, nil
}

// OpenUtilityAllocationDispute 任一方就水电尾款费用分摊提起争议(读数不符/分摊不清)。
// 预留款立即转入人工复核并保持待处理; 触发器同时阻止最终结算与主押金结算确认。
func (s *Service) OpenUtilityAllocationDispute(ctx context.Context, contractID, comment string) (string, error) {
	var did string
	err := s.runTx(ctx, contractID, func(tx pgx.Tx) error {
		if _, err := s.assertParty(tx, ctx, contractID, "TENANT", "LANDLORD", "AGENT", "PLATFORM"); err != nil {
			return err
		}
		var rid, rst string
		if err := tx.QueryRow(ctx,
			`SELECT id::text, status FROM utility_reservations WHERE contract_id=$1`,
			contractID).Scan(&rid, &rst); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if rst == UtilityReserveStatusSettled {
			return fmt.Errorf("%w: 已最终结算", ErrBadState)
		}
		// 同一合同同一预留同时只允许一个未闭合的水电尾款争议(部分唯一索引兜底)。
		var n int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM disputes WHERE contract_id=$1 AND reason='UTILITY_FINAL_SETTLEMENT'
			  AND status IN ('OPEN','EVIDENCE_PENDING','UNDER_REVIEW')`, contractID).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return fmt.Errorf("%w: 水电尾款分摊争议已在处理中", ErrConflict)
		}
		var reserved float64
		if err := tx.QueryRow(ctx,
			`SELECT reserved_amount FROM utility_reservations WHERE id=$1`, rid).Scan(&reserved); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO disputes(contract_id,reason,subject,claimed_amount,opened_by,status)
			VALUES($1,'UTILITY_FINAL_SETTLEMENT',$2,$3,$4,'OPEN') RETURNING id::text`,
			contractID, "退租水电尾款费用分摊/读数争议", reserved, actor(ctx).ID).Scan(&did); err != nil {
			return err
		}
		if err := s.disputeAction(tx, ctx, did, contractID, "OPEN", reserved, nil, comment); err != nil {
			return err
		}
		if err := s.updateReservationStatus(tx, ctx, rid, UtilityReserveStatusManualReview,
			map[string]any{"manual_reason": UtilityManualAllocationDispute}); err != nil {
			return err
		}
		// 与现有争议状态机一致: 已进入扣款待确认阶段的合同同步进入 DISPUTING,
		// 争议闭合时 afterDisputeResolved 会自动回到 DEDUCTION_PROPOSED。
		if _, err := tx.Exec(ctx,
			`UPDATE contracts SET status='DISPUTING' WHERE id=$1 AND status='DEDUCTION_PROPOSED'`,
			contractID); err != nil {
			return err
		}
		c, _ := s.getContract(tx, ctx, contractID)
		other := c.LandlordID
		if actor(ctx).ID == c.LandlordID {
			other = c.TenantID
		}
		_ = s.notify(tx, ctx, contractID, other, "utility_dispute_opened",
			map[string]any{"dispute_id": did, "comment": comment})
		return s.emit(tx, ctx, contractID, "UtilityAllocationDisputeOpened", map[string]any{
			"reservation_id": rid, "dispute_id": did, "comment": comment,
		}, actor(ctx).ID)
	})
	return did, err
}

// ResolveUtilityManual 平台结束人工复核, 让预留恢复到可结算口径。
// 账单延迟且供应商始终不出账时, 可用 SettledAmount 直接核定(自动补一张平台核定单)。
func (s *Service) ResolveUtilityManual(ctx context.Context, contractID, reservationID string,
	in ResolveUtilityManualInput) error {
	return s.runTx(ctx, contractID, func(tx pgx.Tx) error {
		if actor(ctx).Role != "PLATFORM" {
			return fmt.Errorf("%w: 仅平台可结束人工复核", ErrForbidden)
		}
		var rst, reason string
		if err := tx.QueryRow(ctx,
			`SELECT status, COALESCE(manual_reason,'') FROM utility_reservations WHERE id=$1 AND contract_id=$2`,
			reservationID, contractID).Scan(&rst, &reason); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if rst != UtilityReserveStatusManualReview {
			return fmt.Errorf("%w: 当前状态 %s 不在人工复核", ErrBadState, rst)
		}
		if in.Action != "RESUME" {
			return fmt.Errorf("%w: action 仅支持 RESUME", ErrValidation)
		}
		// 分摊争议必须先在争议流程中闭合(ADJUDICATE/协商), 平台才能恢复结算。
		var n int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM disputes WHERE contract_id=$1 AND reason='UTILITY_FINAL_SETTLEMENT'
			  AND status IN ('OPEN','EVIDENCE_PENDING','UNDER_REVIEW')`, contractID).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return fmt.Errorf("%w: 水电尾款分摊争议尚未闭合", ErrUnresolved)
		}

		// 平台核定金额(账单延迟等无账单情形): 补一张平台核定单, 使最终结算始终有账单凭据。
		if in.SettledAmount > 0 {
			var billN int
			if err := tx.QueryRow(ctx,
				`SELECT count(*) FROM utility_bills WHERE reservation_id=$1`, reservationID).
				Scan(&billN); err != nil {
				return err
			}
			if billN == 0 {
				if _, err := tx.Exec(ctx, `
					INSERT INTO utility_bills(contract_id,reservation_id,bill_no,supplier,line_items,
					                          total_amount,settled_amount,note,status,uploaded_by)
					SELECT $1,$2,'MANUAL-'||substr($3::text,1,8),'平台核定(供应商账单延迟)',
					       '[]',$4,$4,$5,'RECEIVED',$6`,
					contractID, reservationID, reservationID, round2(in.SettledAmount),
					in.Note, actor(ctx).ID); err != nil {
					return fmt.Errorf("create manual bill: %w", err)
				}
			} else {
				if _, err := tx.Exec(ctx,
					`UPDATE utility_bills SET settled_amount=$2 WHERE reservation_id=$1 AND status='RECEIVED'`,
					reservationID, round2(in.SettledAmount)); err != nil {
					return err
				}
			}
		} else {
			// 无账单又无核定金额, 无法结算。
			var billN int
			if err := tx.QueryRow(ctx,
				`SELECT count(*) FROM utility_bills WHERE reservation_id=$1 AND status='RECEIVED'`,
				reservationID).Scan(&billN); err != nil {
				return err
			}
			if billN == 0 {
				return fmt.Errorf("%w: 账单仍未到达, 请提供 settled_amount 由平台核定", ErrValidation)
			}
		}
		if err := s.updateReservationStatus(tx, ctx, reservationID,
			UtilityReserveStatusBillReceived, map[string]any{"manual_reason": nil}); err != nil {
			return err
		}
		return s.emit(tx, ctx, contractID, "UtilityManualReviewResolved", map[string]any{
			"reservation_id": reservationID, "settled_amount": in.SettledAmount, "note": in.Note,
		}, actor(ctx).ID)
	})
}

// ---------- 6. 最终结算(唯一一笔最终押金流水, 幂等) ----------

type UtilityFinalResult struct {
	FinalID          string  `json:"final_settlement_id"`
	ReservationID    string  `json:"reservation_id"`
	BillID           string  `json:"bill_id"`
	Idempotent       bool    `json:"idempotent"`
	ReservedAmount   float64 `json:"reserved_amount"`
	ChargeTotal      float64 `json:"charge_total"`
	FromDeposit      float64 `json:"from_deposit"`
	RefundToTenant   float64 `json:"refund_to_tenant"`
	SupplementAmount float64 `json:"supplement_amount"`
	SupplementID     string  `json:"supplement_id,omitempty"`
	SettleKind       string  `json:"settle_kind"`
	Status           string  `json:"status"`
}

// SettleUtilityReservation 按核定费用完成水电尾款最终结算。
// 重复调用(同一账单已结算/补传照片后再调)只返回已有结果, 绝不产生第二笔扣款。
func (s *Service) SettleUtilityReservation(ctx context.Context, contractID, reservationID string,
	note string) (*UtilityFinalResult, error) {
	res := &UtilityFinalResult{}
	err := s.runTx(ctx, contractID, func(tx pgx.Tx) error {
		if _, err := s.assertParty(tx, ctx, contractID, "LANDLORD", "AGENT", "PLATFORM"); err != nil {
			return err
		}
		// 幂等快路径: 已有最终结算 -> 原样返回, 无资金动作。
		var existingID string
		eerr := tx.QueryRow(ctx, `
			SELECT id::text FROM final_utility_settlements WHERE reservation_id=$1`,
			reservationID).Scan(&existingID)
		if eerr == nil {
			r, err := s.loadFinalResult(tx, ctx, existingID)
			if err != nil {
				return err
			}
			r.Idempotent = true
			*res = *r
			return nil
		}
		if !errors.Is(eerr, pgx.ErrNoRows) {
			return eerr
		}

		var rst string
		var reserved float64
		if err := tx.QueryRow(ctx,
			`SELECT status, reserved_amount FROM utility_reservations
			 WHERE id=$1 AND contract_id=$2 FOR UPDATE`,
			reservationID, contractID).Scan(&rst, &reserved); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if rst != UtilityReserveStatusBillReceived {
			return fmt.Errorf("%w: 预留状态=%s, 仅 BILL_RECEIVED 可最终结算(账单未到/待照片复核/人工复核中均不可)",
				ErrBadState, rst)
		}
		// 应用层提前拦截未闭合争议(触发器也会兜底)。
		var dn int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM disputes WHERE contract_id=$1
			  AND status IN ('OPEN','EVIDENCE_PENDING','UNDER_REVIEW')`, contractID).Scan(&dn); err != nil {
			return err
		}
		if dn > 0 {
			return fmt.Errorf("%w: 存在未闭合争议(含水电尾款分摊), 暂缓最终结算", ErrUnresolved)
		}

		var billID string
		var totalAmount, settledAmount *float64
		var lineJSON []byte
		if err := tx.QueryRow(ctx, `
			SELECT id::text, total_amount, settled_amount, line_items
			FROM utility_bills WHERE reservation_id=$1 AND status='RECEIVED'`,
			reservationID).Scan(&billID, &totalAmount, &settledAmount, &lineJSON); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: 缺少在途供应商账单", ErrBadState)
			}
			return err
		}
		charge := 0.0
		if settledAmount != nil {
			charge = *settledAmount
		} else if totalAmount != nil {
			charge = *totalAmount
		}
		charge = round2(charge)

		// 结算口径: 有照片复核记录 -> REVIEW_CROSS_CHECK; 平台核定单/人工改金额/读数无法对应后
		// 人工恢复 -> MANUAL_ADJUDICATED; 其余(无照片无改按实账) -> BILL_ONLY。
		settleKind := "BILL_ONLY"
		var billNo string
		if err := tx.QueryRow(ctx, `SELECT bill_no FROM utility_bills WHERE id=$1`, billID).
			Scan(&billNo); err != nil {
			return err
		}
		var latestReview *string
		if err := tx.QueryRow(ctx, `
			SELECT decision FROM meter_reviews WHERE reservation_id=$1
			ORDER BY created_at DESC LIMIT 1`, reservationID).Scan(&latestReview); err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			latestReview = nil
		}
		switch {
		case len(billNo) >= 7 && billNo[:7] == "MANUAL-":
			settleKind = "MANUAL_ADJUDICATED" // 供应商始终未出账, 平台核定
		case latestReview != nil && *latestReview == "UNMATCHABLE":
			settleKind = "MANUAL_ADJUDICATED" // 读数无法对应, 人工复核后恢复
		case latestReview != nil:
			settleKind = "REVIEW_CROSS_CHECK" // MATCH/ADOPT_PHOTO 交叉复核
		case settledAmount != nil && totalAmount != nil &&
			absF(*settledAmount-*totalAmount) > 0.001:
			settleKind = "MANUAL_ADJUDICATED" // 人工改过账单金额
		}
		var reviewID *string
		if latestReview != nil {
			if err := tx.QueryRow(ctx, `
				SELECT id::text FROM meter_reviews WHERE reservation_id=$1
				ORDER BY created_at DESC LIMIT 1`, reservationID).Scan(&reviewID); err != nil {
				return err
			}
		}

		var balance, hard float64
		if err := tx.QueryRow(ctx,
			`SELECT balance, hard_hold FROM escrow_accounts WHERE contract_id=$1 FOR UPDATE`,
			contractID).Scan(&balance, &hard); err != nil {
			return err
		}
		if hard+0.001 < reserved {
			return fmt.Errorf("CONFLICT: hard_hold %.2f 小于预留 %.2f, 资金状态异常", hard, reserved)
		}

		fromDeposit := round2(minF(charge, reserved))
		refund := round2(reserved - fromDeposit)
		supplement := round2(charge - fromDeposit)
		if supplement < 0 {
			supplement = 0
		}

		// 托管内承担部分: 预留(hard_hold)支付给供应商/房东。
		if fromDeposit > 0 {
			if err := s.ledger(tx, ctx, contractID, "UTILITY_FINAL_SETTLE", fromDeposit,
				-fromDeposit, -fromDeposit, 0, "utility_reservation", reservationID,
				fmt.Sprintf("水电尾款最终结算: 核定 %.2f 元, 从预留押金支付", charge),
				"utility-final-pay:"+reservationID); err != nil {
				return err
			}
		}
		// 多出的预留立即退还租客(预留原本就占用 hard_hold, 退款同时释放该部分 hard_hold)。
		if refund > 0 {
			if err := s.ledger(tx, ctx, contractID, "UTILITY_FINAL_SETTLE", refund,
				-refund, -refund, 0, "utility_reservation", reservationID,
				"水电尾款预留余款退还租客", "utility-final-refund:"+reservationID); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx,
			`UPDATE escrow_accounts SET released_total=released_total+$2 WHERE contract_id=$1`,
			contractID, reserved); err != nil {
			return err
		}

		breakdown := json.RawMessage(lineJSON)
		if len(lineJSON) == 0 {
			breakdown = json.RawMessage("[]")
		}
		var finalID string
		if err := tx.QueryRow(ctx, `
			INSERT INTO final_utility_settlements(contract_id,reservation_id,bill_id,meter_review_id,
			                                      reserved_amount,charge_total,from_deposit,
			                                      refund_to_tenant,supplement_amount,
			                                      charge_breakdown,settle_kind,note,created_by)
			VALUES($1,$2,$3,NULLIF($4,'')::uuid,$5,$6,$7,$8,$9,$10,$11,$12,$13)
			RETURNING id::text`,
			contractID, reservationID, billID, derefStr(reviewID),
			reserved, charge, fromDeposit, refund, supplement,
			string(breakdown), settleKind, note, actor(ctx).ID).Scan(&finalID); err != nil {
			return mapPgGuardError(err)
		}
		// 账单标记 ADOPTED; 历史其他账单(理论不存在)留痕。
		if _, err := tx.Exec(ctx,
			`UPDATE utility_bills SET status='ADOPTED' WHERE id=$1`, billID); err != nil {
			return err
		}
		// 不足部分: 发起补缴(不入托管)。
		suppID := ""
		if supplement > 0 {
			if err := tx.QueryRow(ctx, `
				INSERT INTO utility_supplements(contract_id,final_settlement_id,amount,note)
				VALUES($1,$2,$3,$4) RETURNING id::text`,
				contractID, finalID, supplement,
				"水电尾款实际费用超出预留, 向租客发起补缴").Scan(&suppID); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE utility_reservations SET status='SETTLED', finalized_at=now(),
			                                status_updated_at=now()
			WHERE id=$1`, reservationID); err != nil {
			return err
		}

		c, _ := s.getContract(tx, ctx, contractID)
		if refund > 0 {
			_ = s.notify(tx, ctx, contractID, c.TenantID, "utility_refund", map[string]any{
				"amount": refund, "charge": charge, "reserved": reserved,
			})
		}
		if supplement > 0 {
			_ = s.notify(tx, ctx, contractID, c.TenantID, "utility_supplement_requested", map[string]any{
				"amount": supplement, "charge": charge, "reserved": reserved,
			})
		}
		_ = s.notify(tx, ctx, contractID, c.LandlordID, "utility_final_settled", map[string]any{
			"paid": fromDeposit, "charge": charge,
		})
		if err := s.emit(tx, ctx, contractID, "UtilityFinalSettled", map[string]any{
			"reservation_id": reservationID, "final_settlement_id": finalID,
			"bill_id": billID, "settle_kind": settleKind,
			"reserved_amount": reserved, "charge_total": charge,
			"from_deposit": fromDeposit, "refund_to_tenant": refund,
			"supplement_amount": supplement, "supplement_id": suppID,
		}, actor(ctx).ID); err != nil {
			return err
		}
		res = &UtilityFinalResult{
			FinalID: finalID, ReservationID: reservationID, BillID: billID,
			ReservedAmount: reserved, ChargeTotal: charge, FromDeposit: fromDeposit,
			RefundToTenant: refund, SupplementAmount: supplement, SupplementID: suppID,
			SettleKind: settleKind, Status: UtilityReserveStatusSettled,
		}
		return nil
	})
	return res, err
}

func (s *Service) loadFinalResult(tx pgx.Tx, ctx context.Context, finalID string) (*UtilityFinalResult, error) {
	var r UtilityFinalResult
	var suppID *string
	if err := tx.QueryRow(ctx, `
		SELECT id::text, reservation_id::text, COALESCE(bill_id::text,''),
		       reserved_amount, charge_total, from_deposit, refund_to_tenant, supplement_amount,
		       settle_kind, (SELECT id::text FROM utility_supplements WHERE final_settlement_id=$1 LIMIT 1)
		FROM final_utility_settlements WHERE id=$1`, finalID).
		Scan(&r.FinalID, &r.ReservationID, &r.BillID,
			&r.ReservedAmount, &r.ChargeTotal, &r.FromDeposit, &r.RefundToTenant,
			&r.SupplementAmount, &r.SettleKind, &suppID); err != nil {
		return nil, err
	}
	if suppID != nil {
		r.SupplementID = *suppID
	}
	r.Status = UtilityReserveStatusSettled
	return &r, nil
}

// PayUtilitySupplement 租客支付补缴款(不经过托管账户, 直接结清应收)。
func (s *Service) PayUtilitySupplement(ctx context.Context, contractID, supplementID string) error {
	return s.runTx(ctx, contractID, func(tx pgx.Tx) error {
		if _, err := s.assertParty(tx, ctx, contractID, "TENANT", "PLATFORM"); err != nil {
			return err
		}
		var st string
		var amount float64
		if err := tx.QueryRow(ctx, `
			SELECT status, amount FROM utility_supplements
			WHERE id=$1 AND contract_id=$2 FOR UPDATE`,
			supplementID, contractID).Scan(&st, &amount); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if st == "PAID" {
			// 幂等: 重复支付不重复记账。
			return nil
		}
		if st != "PENDING" {
			return fmt.Errorf("%w: 补缴单状态=%s", ErrBadState, st)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE utility_supplements SET status='PAID', paid_by=$2, paid_at=now()
			WHERE id=$1`, supplementID, actor(ctx).ID); err != nil {
			return err
		}
		c, _ := s.getContract(tx, ctx, contractID)
		_ = s.notify(tx, ctx, contractID, c.LandlordID, "utility_supplement_paid",
			map[string]any{"amount": amount})
		return s.emit(tx, ctx, contractID, "UtilitySupplementPaid", map[string]any{
			"supplement_id": supplementID, "amount": amount,
		}, actor(ctx).ID)
	})
}

// ---------- 辅助 ----------

func (s *Service) updateReservationStatus(tx pgx.Tx, ctx context.Context, rid, newStatus string,
	extra map[string]any) error {
	if extra == nil {
		extra = map[string]any{}
	}
	q := `UPDATE utility_reservations SET status=$2, status_updated_at=now()`
	args := []any{rid, newStatus}
	if _, ok := extra["manual_reason"]; ok {
		q += `, manual_reason=NULLIF($3,'')`
		args = append(args, extra["manual_reason"])
	}
	if _, ok := extra["bill_overdue"]; ok {
		q += fmt.Sprintf(`, bill_overdue=$%d`, len(args)+1)
		args = append(args, extra["bill_overdue"])
	}
	q += ` WHERE id=$1`
	_, err := tx.Exec(ctx, q, args...)
	return err
}

// transitionReservation 更新状态并在同一事务执行后续动作。
func (s *Service) transitionReservation(tx pgx.Tx, ctx context.Context, contractID, rid, newStatus string,
	extra map[string]any, _ bool, after func() error) error {
	if err := s.updateReservationStatus(tx, ctx, rid, newStatus, extra); err != nil {
		return err
	}
	return after()
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func absF(a float64) float64 {
	if a < 0 {
		return -a
	}
	return a
}
