package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"deposit-escrow/internal/domain"
	"deposit-escrow/internal/rules"

	"github.com/jackc/pgx/v5"
)

// ---------- 发起验房 ----------

// StartInspection 由房东或中介在租客交房后发起验房。
func (s *Service) StartInspection(ctx context.Context, contractID string) (string, error) {
	var inspID string
	err := s.runTx(ctx, contractID, func(tx pgx.Tx) error {
		status, err := s.assertParty(tx, ctx, contractID, "LANDLORD", "AGENT", "PLATFORM")
		if err != nil {
			return err
		}
		if status == domain.StatusDisputing {
			// 仅允许在"房东逾期不验房"争议打开时补开验房。
			var n int
			if err := tx.QueryRow(ctx, `
				SELECT count(*) FROM disputes WHERE contract_id=$1 AND reason='INSPECTION_DELAY'
				  AND status IN ('OPEN','EVIDENCE_PENDING','UNDER_REVIEW')`, contractID).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				return fmt.Errorf("%w: DISPUTING 状态仅在逾期验房争议下允许补开验房", ErrBadState)
			}
		} else if err := checkStatus(status,
			domain.StatusCheckoutPending, domain.StatusEarlyTerminated); err != nil {
			return err
		}
		c, err := s.getContract(tx, ctx, contractID)
		deadline := nowUTC().AddDate(0, 0, c.InspectionDueDays)
		early := status == domain.StatusEarlyTerminated
		if err := tx.QueryRow(ctx, `
			INSERT INTO inspections(contract_id,initiated_by,status,checkout_handover_id,
			                        inspection_deadline,early_termination)
			SELECT $1,$2,'OPEN',h.id,$3,$4 FROM handovers h
			 WHERE h.contract_id=$1 AND h.phase='CHECKOUT'
			RETURNING id::text`, contractID, actor(ctx).ID, deadline, early).Scan(&inspID); err != nil {
			return fmt.Errorf("start inspection: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE contracts SET status='INSPECTING' WHERE id=$1`, contractID); err != nil {
			return err
		}
		// 房东开始验房, 租客此前因"迟迟不验房"提起的争议自动闭合(无金额影响)。
		if _, err := tx.Exec(ctx, `
			UPDATE disputes SET status='RESOLVED_AGREEMENT', resolved_at=now(),
			                    resolution_note='房东已发起验房, 逾期争议闭合', award_to_landlord=0
			WHERE contract_id=$1 AND reason='INSPECTION_DELAY'
			  AND status IN ('OPEN','EVIDENCE_PENDING','UNDER_REVIEW')`, contractID); err != nil {
			return err
		}
		_ = s.notify(tx, ctx, contractID, c.TenantID, "inspection_started",
			map[string]any{"deadline": deadline.Format(time.RFC3339)})
		return s.emit(tx, ctx, contractID, "InspectionStarted",
			map[string]any{"inspection_id": inspID, "deadline": deadline}, actor(ctx).ID)
	})
	return inspID, err
}

// AutoOpenOverdueInspection 供平台定时任务调用: 超过验房期限房东仍未验房时, 平台代开验房。
func (s *Service) AutoOpenOverdueInspection(ctx context.Context) ([]string, error) {
	if actor(ctx).Role != "PLATFORM" {
		return nil, fmt.Errorf("%w: platform only", ErrForbidden)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT c.id::text FROM contracts c
		JOIN handovers h ON h.contract_id=c.id AND h.phase='CHECKOUT'
		WHERE c.status IN ('CHECKOUT_PENDING','EARLY_TERMINATED','DISPUTING')
		  AND h.occurred_at + make_interval(days => c.inspection_due_days) < now()
		  AND NOT EXISTS (SELECT 1 FROM inspections i WHERE i.contract_id=c.id)
		  -- DISPUTING 仅处理因房东逾期不验房引起的争议, 扣款争议不得被此流程跳过
		  AND (c.status <> 'DISPUTING' OR EXISTS (
		        SELECT 1 FROM disputes d WHERE d.contract_id=c.id AND d.reason='INSPECTION_DELAY'
		          AND d.status IN ('OPEN','EVIDENCE_PENDING','UNDER_REVIEW')))`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	var opened []string
	for _, id := range ids {
		if _, err := s.StartInspection(WithActor(ctx, actor(ctx)), id); err != nil {
			return opened, err
		}
		opened = append(opened, id)
	}
	return opened, nil
}

// ---------- 提交验房结论: 规则引擎生成扣款建议 + 资金软冻结 ----------

type SubmitInspectionInput struct {
	WallStatus      string `json:"wall_status"`
	FloorStatus     string `json:"floor_status"`
	ApplianceStatus string `json:"appliance_status"`
	KeysStatus      string `json:"keys_status"`
	UtilityStatus   string `json:"utility_status"`
	CleaningStatus  string `json:"cleaning_status"`
	Summary         string `json:"summary"`
	// 房东主张的费用报价(必须有票据类证据, 最终仍需租客确认或裁决)
	WallRepairFee     float64  `json:"wall_repair_fee"`
	FloorRepairFee    float64  `json:"floor_repair_fee"`
	ApplianceFee      float64  `json:"appliance_fee"`
	KeyReplacementFee float64  `json:"key_replacement_fee"`
	CleaningFee       float64  `json:"cleaning_fee"`
	EvidenceIDs       []string `json:"evidence_ids"`
}

func (s *Service) SubmitInspection(ctx context.Context, contractID string, in SubmitInspectionInput) (*rules.Report, error) {
	var report *rules.Report
	err := s.runTx(ctx, contractID, func(tx pgx.Tx) error {
		if _, err := s.assertParty(tx, ctx, contractID, "LANDLORD", "AGENT", "PLATFORM"); err != nil {
			return err
		}
		var inspID string
		var st string
		if err := tx.QueryRow(ctx,
			`SELECT id::text, status FROM inspections WHERE contract_id=$1`, contractID).
			Scan(&inspID, &st); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if st != "OPEN" && st != "REINSPECT" && st != "SUBMITTED" {
			return fmt.Errorf("%w: inspection already %s", ErrBadState, st)
		}
		var cst string
		if err := tx.QueryRow(ctx, `SELECT status FROM contracts WHERE id=$1`, contractID).Scan(&cst); err != nil {
			return err
		}
		if err := checkStatus(cst, domain.StatusInspecting, domain.StatusDeductionProposed); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE inspections SET wall_status=$2,floor_status=$3,appliance_status=$4,keys_status=$5,
				utility_status=$6,cleaning_status=$7,summary=$8,status='SUBMITTED',submitted_at=now()
			WHERE id=$1`,
			inspID, in.WallStatus, in.FloorStatus, in.ApplianceStatus, in.KeysStatus,
			in.UtilityStatus, in.CleaningStatus, in.Summary); err != nil {
			return err
		}
		// 复检(REINSPECT)重新生成建议前, 先把上一版仍处于 PROPOSED 的软冻结解冻,
		// 已被租客确认(hard_hold)或已进入争议的款项不受影响。
		priorHold := 0.0
		if st == "REINSPECT" {
			if err := tx.QueryRow(ctx, `
				SELECT COALESCE((SELECT sum(proposed_amount) FROM findings
				                 WHERE contract_id=$1 AND status='PROPOSED'),0)
				     + COALESCE((SELECT sum(amount) FROM charge_items
				                 WHERE contract_id=$1 AND status='PROPOSED'),0)`,
				contractID).Scan(&priorHold); err != nil {
				return err
			}
			if priorHold > 0 {
				if err := s.ledger(tx, ctx, contractID, "HOLD_RELEASE", priorHold, 0, 0, -priorHold,
					"inspection", inspID, "复检: 撤销上一版扣款建议冻结", ""); err != nil {
					return err
				}
			}
		}

		snap, err := s.buildSnapshot(tx, ctx, contractID, in)
		if err != nil {
			return err
		}
		r := rules.Generate(snap)
		report = &r
		aid := actor(ctx).ID
		evLiteral := pgUUIDArrayLiteral(in.EvidenceIDs)

		// 清除上一版未被租客处理的建议(允许 REINSPECT 重新生成); 已进入争议的保留。
		if _, err := tx.Exec(ctx, `
			DELETE FROM findings WHERE contract_id=$1 AND status='PROPOSED' AND source='RULE'`, contractID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			DELETE FROM charge_items WHERE contract_id=$1 AND status='PROPOSED' AND kind IN
			  ('CLEANING','UTILITY_UNPAID','KEY_LOSS')`, contractID); err != nil {
			return err
		}

		total := 0.0
		for _, f := range r.Facts {
			var fid string
			if err := tx.QueryRow(ctx, `
				INSERT INTO findings(contract_id,inspection_id,code,title,detail,basis,
					linked_repair_id,proposed_amount,status,source,created_by,evidence_ids)
				VALUES($1,$2,$3,$4,$5,$6,NULLIF($7,'')::uuid,$8,'PROPOSED','RULE',$9,$10::uuid[])
				RETURNING id::text`,
				contractID, inspID, f.Code, f.Title, f.Detail, f.Basis,
				f.LinkedRepairID, f.Amount, aid, evLiteral).Scan(&fid); err != nil {
				return fmt.Errorf("insert finding: %w", err)
			}
			if f.Deductible && f.Amount > 0 {
				total += f.Amount
			}
		}
		for _, ch := range r.Charges {
			kind := map[string]string{
				rules.ChargeCleaning:       "CLEANING",
				rules.ChargeUtilityArrears: "UTILITY_UNPAID",
				rules.ChargeKeyLoss:        "KEY_LOSS",
			}[ch.Code]
			if _, err := tx.Exec(ctx, `
				INSERT INTO charge_items(contract_id,kind,title,amount,status,created_by,evidence_ids)
				VALUES($1,$2,$3,$4,'PROPOSED',$5,$6::uuid[])`,
				contractID, kind, ch.Title, ch.Amount, aid, evLiteral); err != nil {
				return err
			}
			total += ch.Amount
		}
		total = round2(total)

		// 关键: 建议金额只进入 disputed_hold(软冻结), 在租客确认或争议闭合前任何人都拿不到。
		if total > 0 {
			if err := s.ledger(tx, ctx, contractID, "HOLD_PROPOSE", total, 0, 0, total,
				"inspection", inspID, "验房扣款建议冻结(待租客确认/申诉)", ""); err != nil {
				return err
			}
		}
		// 若此前已有未闭合争议(如退租后、验房前租客提前打开的水电尾款分摊争议),
		// 合同保持 DISPUTING; 争议全部闭合后 afterDisputeResolved 会回到 DEDUCTION_PROPOSED。
		var openDisp int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM disputes WHERE contract_id=$1
			  AND status IN ('OPEN','EVIDENCE_PENDING','UNDER_REVIEW')`, contractID).
			Scan(&openDisp); err != nil {
			return err
		}
		nextContractStatus := domain.StatusDeductionProposed
		if openDisp > 0 {
			nextContractStatus = domain.StatusDisputing
		}
		if _, err := tx.Exec(ctx,
			`UPDATE contracts SET status=$2 WHERE id=$1`, contractID, nextContractStatus); err != nil {
			return err
		}
		c, _ := s.getContract(tx, ctx, contractID)
		_ = s.notify(tx, ctx, contractID, c.TenantID, "deduction_proposed",
			map[string]any{"total": total, "note": "请逐项确认或申诉"})
		return s.emit(tx, ctx, contractID, "InspectionSubmitted", map[string]any{
			"inspection_id":  inspID,
			"proposed_total": total,
			"facts":          len(r.Facts), "charges": len(r.Charges), "notes": r.Notes,
		}, aid)
	})
	return report, err
}

// buildSnapshot 从合同/交割/清单/维修/读数聚合规则引擎输入。
func (s *Service) buildSnapshot(tx pgx.Tx, ctx context.Context, contractID string,
	in SubmitInspectionInput) (rules.Snapshot, error) {
	c, err := s.getContract(tx, ctx, contractID)
	if err != nil {
		return rules.Snapshot{}, err
	}
	var cs string
	var early bool
	err = tx.QueryRow(ctx, `
		SELECT c.status, COALESCE(i.early_termination,false)
		FROM contracts c JOIN inspections i ON i.contract_id=c.id
		WHERE c.id=$1`, contractID).Scan(&cs, &early)
	if err != nil {
		return rules.Snapshot{}, err
	}
	snap := rules.Snapshot{
		DepositAmount: c.DepositAmount, RentAmount: c.RentAmount, NoticeDays: c.NoticeDays,
		EarlyTerminated: early || cs == domain.StatusEarlyTerminated,
		WallStatus:      in.WallStatus, FloorStatus: in.FloorStatus, ApplianceStatus: in.ApplianceStatus,
		KeysStatus: in.KeysStatus, UtilityStatus: in.UtilityStatus, CleaningStatus: in.CleaningStatus,
		WallRepairFee: in.WallRepairFee, FloorRepairFee: in.FloorRepairFee, ApplianceFee: in.ApplianceFee,
		KeyReplacementFee: in.KeyReplacementFee, CleaningFee: in.CleaningFee,
	}
	rows, err := tx.Query(ctx, `
		SELECT name, category, checkin_state FROM inventory_items WHERE contract_id=$1`, contractID)
	if err != nil {
		return snap, err
	}
	defer rows.Close()
	for rows.Next() {
		var name, cat, st string
		if err := rows.Scan(&name, &cat, &st); err != nil {
			return snap, err
		}
		snap.Inventory = append(snap.Inventory,
			rules.InventoryView{Name: name, Category: cat, CheckinState: st})
	}
	rows.Close()

	rrows, err := tx.Query(ctx, `
		SELECT id::text, category, status, cost_amount, tenant_share
		FROM repairs WHERE contract_id=$1 ORDER BY occurred_at`, contractID)
	if err != nil {
		return snap, err
	}
	defer rrows.Close()
	for rrows.Next() {
		var rv rules.RepairView
		if err := rrows.Scan(&rv.ID, &rv.Category, &rv.Status, &rv.CostAmount, &rv.TenantShare); err != nil {
			return snap, err
		}
		snap.Repairs = append(snap.Repairs, rv)
	}
	rrows.Close()

	// 水电煤区间应结总额
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(sum(billed_amount),0) FROM utility_readings
		WHERE contract_id=$1 AND phase='CHECKOUT'`, contractID).Scan(&snap.UtilityBilledTotal); err != nil {
		return snap, err
	}
	// 退租时已就"最后一期未出账水电账单"预留押金的, 规则引擎不再产出水电欠费扣款建议,
	// 避免与预留/最终结算双重扣款。
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM utility_reservations WHERE contract_id=$1)`,
		contractID).Scan(&snap.UtilityReserved); err != nil {
		return snap, err
	}
	return snap, nil
}

// ---------- 租客确认 / 申诉扣款 ----------

// AcceptItem 租客接受某条扣款建议: disputed_hold -> hard_hold。
func (s *Service) AcceptItem(ctx context.Context, contractID, itemType, itemID string) error {
	return s.runTx(ctx, contractID, func(tx pgx.Tx) error {
		if _, err := s.assertParty(tx, ctx, contractID, "TENANT", "PLATFORM"); err != nil {
			return err
		}
		amount, _, err := s.setItemStatus(tx, ctx, contractID, itemType, itemID, domain.ItemAccepted)
		if err != nil {
			return err
		}
		if amount > 0 {
			if err := s.ledger(tx, ctx, contractID, "HOLD_CONFIRM", amount, 0, amount, -amount,
				itemType, itemID, "租客确认扣款, 软冻结转定论预留", ""); err != nil {
				return err
			}
		}
		return s.emit(tx, ctx, contractID, "DeductionAccepted",
			map[string]any{"item_type": itemType, "item_id": itemID, "amount": amount}, actor(ctx).ID)
	})
}

// ContestItem 租客对扣款建议申诉: 打开争议, 款项保持 disputed_hold, 合同进入 DISPUTING。
type ContestItemInput struct {
	Reason      string   `json:"reason"`
	Subject     string   `json:"subject"`
	Comment     string   `json:"comment"`
	EvidenceIDs []string `json:"evidence_ids"`
}

func (s *Service) ContestItem(ctx context.Context, contractID, itemType, itemID string,
	in ContestItemInput) (string, error) {
	var disputeID string
	err := s.runTx(ctx, contractID, func(tx pgx.Tx) error {
		if _, err := s.assertParty(tx, ctx, contractID, "TENANT", "PLATFORM"); err != nil {
			return err
		}
		amount, code, err := s.setItemStatus(tx, ctx, contractID, itemType, itemID, domain.ItemContested)
		if err != nil {
			return err
		}
		if in.Reason == "" {
			in.Reason = defaultReasonFor(code, itemType)
		}
		if in.Subject == "" {
			in.Subject = "租客申诉: " + code
		}
		var findingID, chargeID *string
		if itemType == "finding" {
			findingID = &itemID
		} else {
			chargeID = &itemID
		}
		findingVal, chargeVal := "", ""
		if findingID != nil {
			findingVal = *findingID
		}
		if chargeID != nil {
			chargeVal = *chargeID
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO disputes(contract_id,reason,subject,claimed_amount,finding_id,charge_id,opened_by,status)
			VALUES($1,$2,$3,$4,NULLIF($5,'')::uuid,NULLIF($6,'')::uuid,$7,'OPEN') RETURNING id::text`,
			contractID, in.Reason, in.Subject, amount,
			findingVal, chargeVal, actor(ctx).ID).Scan(&disputeID); err != nil {
			return fmt.Errorf("open dispute: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE contracts SET status='DISPUTING' WHERE id=$1`, contractID); err != nil {
			return err
		}
		if err := s.disputeAction(tx, ctx, disputeID, contractID, "OPEN", amount, in.EvidenceIDs, in.Comment); err != nil {
			return err
		}
		c, _ := s.getContract(tx, ctx, contractID)
		_ = s.notify(tx, ctx, contractID, c.LandlordID, "dispute_opened",
			map[string]any{"dispute_id": disputeID, "reason": in.Reason, "amount": amount})
		return s.emit(tx, ctx, contractID, "DisputeOpened", map[string]any{
			"dispute_id": disputeID, "reason": in.Reason,
			"item_type": itemType, "item_id": itemID, "claimed_amount": amount,
		}, actor(ctx).ID)
	})
	return disputeID, err
}

// OpenInspectionDelayDispute 租客就房东迟迟不验房提起争议(交房后超期)。
func (s *Service) OpenInspectionDelayDispute(ctx context.Context, contractID, comment string) (string, error) {
	var id string
	err := s.runTx(ctx, contractID, func(tx pgx.Tx) error {
		if _, err := s.assertParty(tx, ctx, contractID, "TENANT", "PLATFORM"); err != nil {
			return err
		}
		var st string
		var overdue bool
		err := tx.QueryRow(ctx, `
			SELECT c.status,
			       h.occurred_at + make_interval(days => c.inspection_due_days) < now()
			FROM contracts c JOIN handovers h ON h.contract_id=c.id AND h.phase='CHECKOUT'
			WHERE c.id=$1`, contractID).Scan(&st, &overdue)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: 尚未交房", ErrBadState)
		}
		if err != nil {
			return err
		}
		if st != domain.StatusCheckoutPending && st != domain.StatusEarlyTerminated {
			return fmt.Errorf("%w: 当前状态 %s 不允许逾期申诉", ErrBadState, st)
		}
		if !overdue {
			return fmt.Errorf("%w: 尚未超过合同约定的验房期限", ErrValidation)
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO disputes(contract_id,reason,subject,opened_by,status)
			VALUES($1,'INSPECTION_DELAY','房东超过约定期限未验房',$2,'OPEN') RETURNING id::text`,
			contractID, actor(ctx).ID).Scan(&id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE contracts SET status='DISPUTING' WHERE id=$1`, contractID); err != nil {
			return err
		}
		if err := s.disputeAction(tx, ctx, id, contractID, "OPEN", 0, nil, comment); err != nil {
			return err
		}
		c, _ := s.getContract(tx, ctx, contractID)
		_ = s.notify(tx, ctx, contractID, c.LandlordID, "inspection_overdue_dispute",
			map[string]any{"dispute_id": id})
		return s.emit(tx, ctx, contractID, "DisputeOpened", map[string]any{
			"dispute_id": id, "reason": "INSPECTION_DELAY",
		}, actor(ctx).ID)
	})
	return id, err
}

// ---------- 争议时间线动作: 补证 / 驳回 / 协商 / 裁决 ----------

type DisputeActionInput struct {
	Action       string   `json:"action"`
	Comment      string   `json:"comment"`
	Amount       float64  `json:"amount"` // PARTIES_AGREE 时使用
	EvidenceIDs  []string `json:"evidence_ids"`
	DeadlineDays int      `json:"deadline_days"` // REQUEST_MORE_EVIDENCE 时使用
}

func (s *Service) DisputeAction(ctx context.Context, contractID, disputeID string,
	in DisputeActionInput) error {
	return s.runTx(ctx, contractID, func(tx pgx.Tx) error {
		a := actor(ctx)
		var reason, status, openedBy string
		var findingID, chargeID *string
		err := tx.QueryRow(ctx, `
			SELECT reason, status, opened_by::text, finding_id::text, charge_id::text
			FROM disputes WHERE id=$1 AND contract_id=$2`,
			disputeID, contractID).Scan(&reason, &status, &openedBy, &findingID, &chargeID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if isClosedDispute(status) {
			return fmt.Errorf("%w: dispute already resolved", ErrBadState)
		}

		switch in.Action {
		case "SUBMIT_EVIDENCE":
			// 双方均可补证
			if _, err := s.assertParty(tx, ctx, contractID); err != nil {
				return err
			}
			newSt := domain.DisputeEvidencePending
			if status == domain.DisputeUnderReview {
				newSt = domain.DisputeUnderReview
			}
			return s.finishAction(tx, ctx, contractID, disputeID, in, newSt, nil)

		case "ACKNOWLEDGE", "REBUT":
			// 房东对租客申诉进行回应/反驳
			if _, err := s.assertParty(tx, ctx, contractID, "LANDLORD", "AGENT", "PLATFORM"); err != nil {
				return err
			}
			return s.finishAction(tx, ctx, contractID, disputeID, in, domain.DisputeUnderReview, nil)

		case "REQUEST_MORE_EVIDENCE":
			if a.Role != "PLATFORM" {
				return fmt.Errorf("%w: only platform can request more evidence", ErrForbidden)
			}
			var deadline *time.Time
			if in.DeadlineDays > 0 {
				d := nowUTC().AddDate(0, 0, in.DeadlineDays)
				deadline = &d
			}
			if err := s.finishAction(tx, ctx, contractID, disputeID, in,
				domain.DisputeEvidencePending, nil); err != nil {
				return err
			}
			if deadline != nil {
				_, err := tx.Exec(ctx, `UPDATE disputes SET deadline=$2 WHERE id=$1`, disputeID, deadline)
				return err
			}
			return nil

		case "WITHDRAW":
			// 申诉方撤回 = 接受房东原主张: 款项全额转 hard_hold。
			if openedBy != a.ID && a.Role != "PLATFORM" {
				return fmt.Errorf("%w: only opener can withdraw", ErrForbidden)
			}
			return s.resolveByAmount(tx, ctx, contractID, disputeID, -1, "RESOLVED_AGREEMENT",
				"申诉撤回, 按房东原主张扣款", in)

		case "PARTIES_AGREE":
			// 协商一致: 以双方同意的金额定论。
			if _, err := s.assertParty(tx, ctx, contractID); err != nil {
				return err
			}
			in.Amount = round2(in.Amount)
			return s.resolveByAmount(tx, ctx, contractID, disputeID, in.Amount,
				"RESOLVED_AGREEMENT", "双方协商一致", in)

		case "ESCALATE":
			if _, err := s.assertParty(tx, ctx, contractID); err != nil {
				return err
			}
			return s.finishAction(tx, ctx, contractID, disputeID, in,
				domain.DisputeUnderReview, nil)

		case "ADJUDICATE":
			if a.Role != "PLATFORM" {
				return fmt.Errorf("%w: adjudication is platform-only", ErrForbidden)
			}
			in.Amount = round2(in.Amount)
			return s.resolveByAmount(tx, ctx, contractID, disputeID, in.Amount,
				"RESOLVED_ADJUDICATION", "平台裁决: "+in.Comment, in)

		default:
			return fmt.Errorf("%w: unknown action %s", ErrValidation, in.Action)
		}
	})
}

// resolveByAmount 以"支持房东金额 award"闭合争议并联动资金与扣款项状态。
// award<0 表示按原主张全额(撤回申诉)。
func (s *Service) resolveByAmount(tx pgx.Tx, ctx context.Context, contractID, disputeID string,
	award float64, resolveStatus, note string, in DisputeActionInput) error {
	var reason, fStr, cStr string
	var proposed float64
	if err := tx.QueryRow(ctx, `
		SELECT reason, COALESCE(finding_id::text,''), COALESCE(charge_id::text,'')
		FROM disputes WHERE id=$1`, disputeID).Scan(&reason, &fStr, &cStr); err != nil {
		return err
	}
	itemType, itemID := "", ""
	if fStr != "" {
		itemType, itemID = "finding", fStr
		if err := tx.QueryRow(ctx,
			`SELECT proposed_amount FROM findings WHERE id=$1`, itemID).Scan(&proposed); err != nil {
			return err
		}
	} else if cStr != "" {
		itemType, itemID = "charge", cStr
		if err := tx.QueryRow(ctx,
			`SELECT amount FROM charge_items WHERE id=$1`, itemID).Scan(&proposed); err != nil {
			return err
		}
	}
	// INSPECTION_DELAY 等不挂款项的争议: 直接闭合, 无资金动作。
	if itemType == "" {
		if resolveStatus == "RESOLVED_ADJUDICATION" {
			if _, err := tx.Exec(ctx, `
				UPDATE disputes SET status=$2, resolution_note=$3, resolved_at=now(),
				                    adjudicated_by=$4
				WHERE id=$1`, disputeID, resolveStatus, note, actor(ctx).ID); err != nil {
				return err
			}
		} else if _, err := tx.Exec(ctx, `
			UPDATE disputes SET status=$2, resolution_note=$3, resolved_at=now()
			WHERE id=$1`, disputeID, resolveStatus, note); err != nil {
			return err
		}
		if err := s.disputeAction(tx, ctx, disputeID, contractID, in.Action, 0,
			in.EvidenceIDs, in.Comment); err != nil {
			return err
		}
		return s.afterDisputeResolved(tx, ctx, contractID, disputeID)
	}

	if award < 0 {
		award = proposed
	}
	if award < 0 || award > proposed+0.001 {
		return fmt.Errorf("%w: award %.2f out of range [0,%.2f]", ErrValidation, award, proposed)
	}
	award = round2(award)
	freed := round2(proposed - award)

	// 资金联动: 原额在 disputed_hold。支持部分转 hard_hold, 未支持部分立即解冻(变可退余额)。
	if award > 0 {
		if err := s.ledger(tx, ctx, contractID, "HOLD_CONFIRM", award, 0, award, -award,
			itemType, itemID, "争议定论支持房东部分: "+note, ""); err != nil {
			return err
		}
	}
	if freed > 0 {
		if err := s.ledger(tx, ctx, contractID, "HOLD_RELEASE", freed, 0, 0, -freed,
			itemType, itemID, "争议定论撤销的扣款预留: "+note, ""); err != nil {
			return err
		}
	}

	// 扣款项终态: 平台裁决全额驳回=OVERTURNED, 裁决支持=UPHELD; 协商一致=ACCEPTED/REJECTED。
	itemStatus := domain.ItemAccepted
	switch {
	case award <= 0 && resolveStatus == "RESOLVED_ADJUDICATION":
		itemStatus = domain.ItemOverturned
	case award <= 0:
		itemStatus = domain.ItemRejected
	case resolveStatus == "RESOLVED_ADJUDICATION":
		itemStatus = domain.ItemUpheld
	}
	if err := s.updateItemAmount(tx, ctx, itemType, itemID, award, itemStatus); err != nil {
		return err
	}
	if resolveStatus == "RESOLVED_ADJUDICATION" {
		if _, err := tx.Exec(ctx, `
			UPDATE disputes SET status=$2, award_to_landlord=$3, award_to_tenant=$4,
			                    resolution_note=$5, resolved_at=now(), adjudicated_by=$6
			WHERE id=$1`, disputeID, resolveStatus, award, freed, note, actor(ctx).ID); err != nil {
			return err
		}
	} else if _, err := tx.Exec(ctx, `
		UPDATE disputes SET status=$2, award_to_landlord=$3, award_to_tenant=$4,
		                    resolution_note=$5, resolved_at=now()
		WHERE id=$1`, disputeID, resolveStatus, award, freed, note); err != nil {
		return err
	}
	if err := s.disputeAction(tx, ctx, disputeID, contractID, in.Action, award,
		in.EvidenceIDs, in.Comment); err != nil {
		return err
	}
	if err := s.emit(tx, ctx, contractID, "DisputeResolved", map[string]any{
		"dispute_id": disputeID, "resolution": resolveStatus,
		"award_to_landlord": award, "released": freed, "reason": reason,
	}, actor(ctx).ID); err != nil {
		return err
	}
	return s.afterDisputeResolved(tx, ctx, contractID, disputeID)
}

// afterDisputeResolved 全部争议闭合后, 合同状态从 DISPUTING 回到 DEDUCTION_PROPOSED 等待结算。
func (s *Service) afterDisputeResolved(tx pgx.Tx, ctx context.Context, contractID, disputeID string) error {
	var n int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM disputes WHERE contract_id=$1
		  AND status IN ('OPEN','EVIDENCE_PENDING','UNDER_REVIEW')`, contractID).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		if _, err := tx.Exec(ctx, `
			UPDATE contracts SET status='DEDUCTION_PROPOSED' WHERE id=$1 AND status='DISPUTING'`,
			contractID); err != nil {
			return err
		}
		if err := s.emit(tx, ctx, contractID, "AllDisputesResolved",
			map[string]any{"contract_id": contractID}, ""); err != nil {
			return err
		}
	}
	return nil
}

// ---------- 结算与押金释放 ----------

// ProposeSettlement 依据 escrow 当前定论金额生成结算单。
func (s *Service) ProposeSettlement(ctx context.Context, contractID, note string) (string, error) {
	var id string
	err := s.runTx(ctx, contractID, func(tx pgx.Tx) error {
		if _, err := s.assertParty(tx, ctx, contractID, "LANDLORD", "AGENT", "PLATFORM"); err != nil {
			return err
		}
		var st string
		if err := tx.QueryRow(ctx, `SELECT status FROM contracts WHERE id=$1`, contractID).Scan(&st); err != nil {
			return err
		}
		if err := checkStatus(st, domain.StatusDeductionProposed, domain.StatusDisputing); err != nil {
			return err
		}
		var balance, hard, disputed, deposit float64
		if err := tx.QueryRow(ctx, `
			SELECT e.balance, e.hard_hold, e.disputed_hold, c.deposit_amount
			FROM escrow_accounts e JOIN contracts c ON c.id=e.contract_id
			WHERE e.contract_id=$1`, contractID).
			Scan(&balance, &hard, &disputed, &deposit); err != nil {
			return err
		}
		if disputed > 0 {
			return fmt.Errorf("%w: 仍有 %.2f 元扣款项处于待确认/争议中, 不能生成结算",
				ErrUnresolved, disputed)
		}
		// 水电尾款预留尚未最终结算时, 不生成主押金结算(预留也是 hard_hold 的一部分,
		// 必须先多退少补完成唯一一笔水电最终流水; DB 触发器在 CONFIRMED 时再次兜底)。
		var pendN int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM utility_reservations
			WHERE contract_id=$1 AND status <> 'SETTLED'`, contractID).Scan(&pendN); err != nil {
			return err
		}
		if pendN > 0 {
			return fmt.Errorf("%w: 水电尾款预留尚未最终结算(多退少补), 主押金结算暂缓",
				ErrUnresolved)
		}
		// 汇总定论项
		var fids, cids []string
		fr, err := tx.Query(ctx, `
			SELECT id::text FROM findings WHERE contract_id=$1
			 AND status IN ('ACCEPTED','UPHELD')`, contractID)
		if err != nil {
			return err
		}
		defer fr.Close()
		for fr.Next() {
			var v string
			fr.Scan(&v)
			fids = append(fids, v)
		}
		fr.Close()
		cr, err := tx.Query(ctx, `
			SELECT id::text FROM charge_items WHERE contract_id=$1
			 AND status IN ('ACCEPTED','UPHELD')`, contractID)
		if err != nil {
			return err
		}
		defer cr.Close()
		for cr.Next() {
			var v string
			cr.Scan(&v)
			cids = append(cids, v)
		}
		cr.Close()

		// 水电尾款已通过"唯一一笔最终流水"出托管的部分:
		// utilPaid 已付给供应商/房东, utilRefunded 已退还租客, 二者都不再在 escrow.balance 里,
		// 但主结算单必须体现完整押金去向(CHECK: deducted + returned = deposit)。
		var utilPaid, utilRefunded float64
		if err := tx.QueryRow(ctx, `
			SELECT COALESCE(sum(from_deposit),0), COALESCE(sum(refund_to_tenant),0)
			FROM final_utility_settlements WHERE contract_id=$1`,
			contractID).Scan(&utilPaid, &utilRefunded); err != nil {
			return err
		}
		deductedTotal := round2(hard + utilPaid)
		return2tenant := round2(balance - hard + utilRefunded)
		if _, err := tx.Exec(ctx, `
			INSERT INTO deductions(contract_id,total_amount,status,finding_ids,charge_ids,created_by)
			VALUES($1,$2,'PROPOSED',$3::uuid[],$4::uuid[],$5)
			ON CONFLICT (contract_id) DO UPDATE
			  SET total_amount=EXCLUDED.total_amount, status='PROPOSED',
			      finding_ids=EXCLUDED.finding_ids, charge_ids=EXCLUDED.charge_ids,
			      approved_at=NULL`,
			contractID, deductedTotal, pgUUIDArrayLiteral(fids), pgUUIDArrayLiteral(cids),
			actor(ctx).ID); err != nil {
			return fmt.Errorf("upsert deduction: %w", err)
		}
		err = tx.QueryRow(ctx, `
			INSERT INTO settlements(contract_id,deposit_amount,deducted_total,returned_to_tenant,
			                        paid_to_landlord,status,note,finding_ids,charge_ids)
			VALUES($1,$2,$3,$4,$5,'PROPOSED',$6,$7::uuid[],$8::uuid[])
			ON CONFLICT (contract_id) DO UPDATE
			  SET deposit_amount=EXCLUDED.deposit_amount, deducted_total=EXCLUDED.deducted_total,
			      returned_to_tenant=EXCLUDED.returned_to_tenant, paid_to_landlord=EXCLUDED.paid_to_landlord,
			      status='PROPOSED', note=EXCLUDED.note,
			      tenant_confirmed_by=NULL, landlord_confirmed_by=NULL,
			      tenant_confirmed_at=NULL, landlord_confirmed_at=NULL,
			      finding_ids=EXCLUDED.finding_ids, charge_ids=EXCLUDED.charge_ids
			RETURNING id::text`,
			contractID, deposit, deductedTotal, return2tenant, deductedTotal, note,
			pgUUIDArrayLiteral(fids), pgUUIDArrayLiteral(cids)).Scan(&id)
		if err != nil {
			return fmt.Errorf("upsert settlement: %w", err)
		}
		return s.emit(tx, ctx, contractID, "SettlementProposed", map[string]any{
			"settlement_id": id, "deducted_total": deductedTotal,
			"returned_to_tenant": return2tenant,
			"utility_paid":       utilPaid, "utility_refunded": utilRefunded,
		}, actor(ctx).ID)
	})
	return id, err
}

// ConfirmSettlement 租客或房东确认结算; 双方确认后立即执行资金释放。
func (s *Service) ConfirmSettlement(ctx context.Context, contractID string) error {
	return s.runTx(ctx, contractID, func(tx pgx.Tx) error {
		a := actor(ctx)
		var tenantOK, landlordOK bool
		if _, err := s.assertParty(tx, ctx, contractID, "TENANT", "LANDLORD", "AGENT", "PLATFORM"); err != nil {
			return err
		}
		c, err := s.getContract(tx, ctx, contractID)
		if err != nil {
			return err
		}
		isLandlordSide := a.ID == c.LandlordID || a.Role == "LANDLORD" || a.Role == "AGENT" || a.Role == "PLATFORM"
		isTenantSide := a.ID == c.TenantID || a.Role == "TENANT" || a.Role == "PLATFORM"
		if !isLandlordSide && !isTenantSide {
			return ErrForbidden
		}
		var sid string
		var status string
		if err := tx.QueryRow(ctx,
			`SELECT id::text, status FROM settlements WHERE contract_id=$1`, contractID).
			Scan(&sid, &status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: settlement not proposed yet", ErrBadState)
			}
			return err
		}
		if status == "EXECUTED" {
			return fmt.Errorf("%w: settlement already executed", ErrBadState)
		}
		if isTenantSide && a.Role != "PLATFORM" {
			tenantOK = true
		}
		if isLandlordSide && a.Role != "PLATFORM" {
			landlordOK = true
		}
		// 平台确认视为双方确认
		if a.Role == "PLATFORM" {
			tenantOK, landlordOK = true, true
		}
		if tenantOK {
			if _, err := tx.Exec(ctx,
				`UPDATE settlements SET tenant_confirmed_by=$2, tenant_confirmed_at=now() WHERE id=$1`,
				sid, a.ID); err != nil {
				return err
			}
		}
		if landlordOK {
			if _, err := tx.Exec(ctx,
				`UPDATE settlements SET landlord_confirmed_by=$2, landlord_confirmed_at=now() WHERE id=$1`,
				sid, a.ID); err != nil {
				return err
			}
		}

		var both bool
		if err := tx.QueryRow(ctx, `
			SELECT tenant_confirmed_at IS NOT NULL AND landlord_confirmed_at IS NOT NULL
			FROM settlements WHERE id=$1`, sid).Scan(&both); err != nil {
			return err
		}
		if !both {
			return s.emit(tx, ctx, contractID, "SettlementConfirmed",
				map[string]any{"side": map[bool]string{true: "tenant", false: "landlord"}[tenantOK && !landlordOK]}, a.ID)
		}

		// 双方确认 -> CONFIRMED (DB 触发器校验: 无未闭合争议 + 金额==hard_hold) -> 执行释放。
		if _, err := tx.Exec(ctx, `UPDATE settlements SET status='CONFIRMED' WHERE id=$1`, sid); err != nil {
			return mapPgGuardError(err)
		}
		// 扣款单同步定论 APPROVED(扣款在同一事务内执行, 单方无法完成)。
		if _, err := tx.Exec(ctx, `
			UPDATE deductions SET status='APPROVED', approved_at=now()
			WHERE contract_id=$1`, contractID); err != nil {
			return err
		}
		return s.executeRelease(tx, ctx, contractID, sid)
	})
}

// executeRelease 执行最终放款: hard_hold 部分打给房东, 其余退还租客。
func (s *Service) executeRelease(tx pgx.Tx, ctx context.Context, contractID, sid string) error {
	var balance, hard float64
	if err := tx.QueryRow(ctx,
		`SELECT balance, hard_hold FROM escrow_accounts WHERE contract_id=$1 FOR UPDATE`,
		contractID).Scan(&balance, &hard); err != nil {
		return err
	}
	toTenant := round2(balance - hard)
	if hard > 0 {
		if err := s.ledger(tx, ctx, contractID, "DEDUCT", hard, -hard, -hard, 0,
			"settlement", sid, "定论扣款支付给房东", ""); err != nil {
			return err
		}
	}
	if toTenant > 0 {
		if err := s.ledger(tx, ctx, contractID, "RELEASE_TO_TENANT", toTenant, -toTenant, 0, 0,
			"settlement", sid, "押金余额退还租客", ""); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE escrow_accounts SET released_total = released_total + $2 WHERE contract_id=$1`,
		contractID, balance); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE settlements SET status='EXECUTED', executed_at=now() WHERE id=$1`, sid); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE contracts SET status='RELEASED' WHERE id=$1`, contractID); err != nil {
		return err
	}
	c, _ := s.getContract(tx, ctx, contractID)
	_ = s.notify(tx, ctx, contractID, c.TenantID, "deposit_returned",
		map[string]any{"amount": toTenant})
	_ = s.notify(tx, ctx, contractID, c.LandlordID, "deduction_paid",
		map[string]any{"amount": hard})
	return s.emit(tx, ctx, contractID, "DepositReleased", map[string]any{
		"settlement_id": sid, "to_tenant": toTenant, "to_landlord": hard,
	}, actor(ctx).ID)
}

// ---------- 辅助 ----------

func (s *Service) setItemStatus(tx pgx.Tx, ctx context.Context, contractID, itemType, itemID string,
	newStatus string) (amount float64, code string, err error) {
	switch itemType {
	case "finding":
		q := `UPDATE findings SET status=$2, decided_by=$3, decided_at=now()
		      WHERE contract_id=$4 AND id=$1 AND status='PROPOSED'
		      RETURNING proposed_amount, code`
		err = tx.QueryRow(ctx, q, itemID, newStatus, actor(ctx).ID, contractID).
			Scan(&amount, &code)
	case "charge":
		q := `UPDATE charge_items SET status=$2
		      WHERE contract_id=$3 AND id=$1 AND status='PROPOSED'
		      RETURNING amount, kind`
		err = tx.QueryRow(ctx, q, itemID, newStatus, contractID).Scan(&amount, &code)
	default:
		return 0, "", fmt.Errorf("%w: item_type must be finding|charge", ErrValidation)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", fmt.Errorf("%w: item not found or already decided", ErrBadState)
	}
	return amount, code, err
}

func (s *Service) updateItemAmount(tx pgx.Tx, ctx context.Context, itemType, itemID string,
	amount float64, status string) error {
	switch itemType {
	case "finding":
		_, err := tx.Exec(ctx, `UPDATE findings SET proposed_amount=$2,status=$3,decided_at=now()
			WHERE id=$1`, itemID, amount, status)
		return err
	case "charge":
		_, err := tx.Exec(ctx, `UPDATE charge_items SET amount=$2,status=$3 WHERE id=$1`,
			itemID, amount, status)
		return err
	}
	return fmt.Errorf("%w: bad item type", ErrValidation)
}

func (s *Service) disputeAction(tx pgx.Tx, ctx context.Context, disputeID, contractID, action string,
	amount float64, evidence []string, comment string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO dispute_actions(dispute_id,contract_id,actor_id,action,amount_effect,evidence_ids,comment)
		VALUES($1,$2,$3,$4,$5,$6::uuid[],$7)`,
		disputeID, contractID, actor(ctx).ID, action, amount, pgUUIDArrayLiteral(evidence), comment)
	return err
}

func (s *Service) finishAction(tx pgx.Tx, ctx context.Context, contractID, disputeID string,
	in DisputeActionInput, newStatus string, _ any) error {
	if _, err := tx.Exec(ctx, `UPDATE disputes SET status=$2 WHERE id=$1`, disputeID, newStatus); err != nil {
		return err
	}
	if err := s.disputeAction(tx, ctx, disputeID, contractID, in.Action, in.Amount,
		in.EvidenceIDs, in.Comment); err != nil {
		return err
	}
	return s.emit(tx, ctx, contractID, "DisputeAction", map[string]any{
		"dispute_id": disputeID, "action": in.Action, "status": newStatus,
	}, actor(ctx).ID)
}

func isClosedDispute(st string) bool {
	return st == domain.DisputeResolvedAdjud || st == domain.DisputeResolvedAgree ||
		st == domain.DisputeWithdrawn
}

func defaultReasonFor(code, itemType string) string {
	switch code {
	case "WALL_DAMAGE":
		return "WALL_DAMAGE_VS_WEAR"
	case "FLOOR_DAMAGE":
		return "FLOOR_DAMAGE_VS_WEAR"
	}
	if itemType == "charge" {
		switch code {
		case "CLEANING":
			return "CLEANING_FEE"
		case "UTILITY_UNPAID":
			return "UTILITY_ARREARS"
		case "KEY_LOSS":
			return "KEY_LOSS"
		}
	}
	if code == "APPLIANCE_TENANT_LIABLE" || code == "APPLIANCE_RESPONSIBILITY_UNCLEAR" {
		return "APPLIANCE_RESPONSIBILITY"
	}
	return "OTHER"
}

func mapPgGuardError(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if strings.Contains(msg, "CONFLICT_OPEN_DISPUTE") {
		return fmt.Errorf("%w: 存在未闭合争议, 结算/释放被阻止", ErrUnresolved)
	}
	if strings.Contains(msg, "CONFLICT_UTILITY_PENDING") {
		return fmt.Errorf("%w: 水电尾款预留尚未最终结算, 主押金结算被阻止", ErrUnresolved)
	}
	if strings.Contains(msg, "CONFLICT_HOLD_MISMATCH") {
		return fmt.Errorf("%w: 结算金额与定论预留不一致", ErrConflict)
	}
	if strings.Contains(msg, "CONFLICT_UTILITY_RESERVE_MISMATCH") ||
		strings.Contains(msg, "CONFLICT_UTILITY_LEDGER_MISMATCH") {
		return fmt.Errorf("%w: 水电最终结算金额勾稽不一致", ErrConflict)
	}
	if strings.Contains(msg, "CONFLICT_UTILITY_BAD_STATE") {
		return fmt.Errorf("%w: 水电预留状态不允许最终结算", ErrBadState)
	}
	return err
}

// pgUUIDArrayLiteral 把 UUID 字符串列表编码成 PostgreSQL uuid[] 字面量。
// 调用方须把参数以 $n::uuid[] 方式传入。
func pgUUIDArrayLiteral(ids []string) string {
	if len(ids) == 0 {
		return "{}"
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, id := range ids {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strings.TrimSpace(id))
	}
	b.WriteByte('}')
	return b.String()
}
