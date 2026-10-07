// Package rules 是退租验房的可扣款项建议引擎。
//
// 它只读证据快照(合同 + 入住交割 + 家具家电清单 + 维修记录 + 退租验房结论 + 水电读数),
// 产出"建议"而非"定论": 金额只进入 disputed_hold, 需双方接受或平台裁决后才成 hard_hold。
// 对自然磨损、入住前已损、房东责任维修, 引擎显式产出 0 元建议以保护租客;
// 对家电维修责任不清这类证据不足情形, 产出 UNCLEAR 建议并提示必须先闭合责任争议。
package rules

import "fmt"

// 建议项编码
const (
	CodeWallDamage                = "WALL_DAMAGE"
	CodeWallNormalWear            = "WALL_NORMAL_WEAR"
	CodeFloorDamage               = "FLOOR_DAMAGE"
	CodeFloorNormalWear           = "FLOOR_NORMAL_WEAR"
	CodePreExistingDamage         = "PRE_EXISTING_DAMAGE"
	CodeApplianceTenantLiable     = "APPLIANCE_TENANT_LIABLE"
	CodeApplianceLandlordLiable   = "APPLIANCE_LANDLORD_LIABLE"
	CodeApplianceUnclear          = "APPLIANCE_RESPONSIBILITY_UNCLEAR"
	CodeEarlyTerminationLiability = "EARLY_TERMINATION_LIABILITY"

	ChargeCleaning       = "CLEANING_FEE"
	ChargeUtilityArrears = "UTILITY_ARREARS"
	ChargeKeyLoss        = "KEY_LOSS"
)

// InventoryView 是清单条目的判定视图。
type InventoryView struct {
	Name         string
	Category     string
	CheckinState string // NEW/GOOD/WORN/DAMAGED/MISSING
}

// RepairView 是维修记录的判定视图。
type RepairView struct {
	ID          string
	Category    string
	Status      string
	CostAmount  float64
	TenantShare float64
}

// Snapshot 是验房时刻与扣款判定有关的全部证据。
type Snapshot struct {
	DepositAmount   float64
	RentAmount      float64
	NoticeDays      int
	EarlyTerminated bool

	WallStatus      string // GOOD/NORMAL_WEAR/DAMAGED
	FloorStatus     string
	ApplianceStatus string // GOOD/NORMAL_WEAR/DAMAGED/MISSING
	KeysStatus      string // RETURNED/MISSING/DAMAGED
	UtilityStatus   string // SETTLED/UNPAID
	CleaningStatus  string // CLEAN/NEEDS_CLEANING

	Inventory []InventoryView
	Repairs   []RepairView

	// 房东主张的费用报价(需票据佐证, 仍需租客确认或裁决)
	WallRepairFee     float64
	FloorRepairFee    float64
	ApplianceFee      float64
	KeyReplacementFee float64
	CleaningFee       float64
	// 水电煤区间应结金额(按入住/退租读数与单价算出)
	UtilityBilledTotal float64
	// 退租时最后一期水电账单尚未出, 平台已就水电尾款做押金预留:
	// 引擎不再生成 UTILITY_ARREARS 扣款建议(避免与预留/最终结算双重扣款)。
	UtilityReserved bool
}

// Suggestion 是一条可扣款项建议(findings 表)。
type Suggestion struct {
	Code           string  `json:"code"`
	Title          string  `json:"title"`
	Detail         string  `json:"detail"`
	Basis          string  `json:"basis"`
	LinkedRepairID string  `json:"linked_repair_id,omitempty"`
	Amount         float64 `json:"amount"`
	// Deductible=false 表示引擎判定不应扣款(自然磨损/入住前损坏/房东责任)。
	Deductible bool `json:"deductible"`
}

// ChargeSuggestion 是清洁/欠费/钥匙类费用主张(charge_items 表)。
type ChargeSuggestion struct {
	Code   string  `json:"code"`
	Title  string  `json:"title"`
	Detail string  `json:"detail"`
	Basis  string  `json:"basis"`
	Amount float64 `json:"amount"`
}

// Report 是引擎产物。
type Report struct {
	Facts   []Suggestion // 含 0 元的保护性判定(自然磨损等)
	Charges []ChargeSuggestion
	Notes   []string
}

// Generate 依据证据快照生成扣款建议。纯函数, 无副作用。
func Generate(s Snapshot) Report {
	r := Report{}

	r.Facts = append(r.Facts, surfaceRules(s, "WALL", s.WallStatus, CodeWallDamage, CodeWallNormalWear,
		"墙面", s.WallRepairFee)...)
	r.Facts = append(r.Facts, surfaceRules(s, "FLOOR", s.FloorStatus, CodeFloorDamage, CodeFloorNormalWear,
		"地板", s.FloorRepairFee)...)

	// 家电: 需要结合维修责任记录。
	r.Facts = append(r.Facts, applianceRules(s)...)

	// 租客提前退租: 按合同通知期计算空置损失, 以押金为上限。
	if s.EarlyTerminated {
		amt := s.RentAmount * float64(s.NoticeDays) / 30.0
		amt = cap0(amt)
		r.Facts = append(r.Facts, Suggestion{
			Code:       CodeEarlyTerminationLiability,
			Title:      "租客提前退租空置损失(建议)",
			Detail:     fmt.Sprintf("按合同约定提前 %d 天通知期、月租 %.2f 元测算: %.2f 元", s.NoticeDays, s.RentAmount, amt),
			Basis:      "合同通知期条款(notice_days) + 提前退租事实; 实际损失以房东重新出租日期为准, 租客可举证减损",
			Amount:     amt,
			Deductible: true,
		})
	}

	// 费用类
	if s.KeysStatus == "MISSING" || s.KeysStatus == "DAMAGED" {
		r.Charges = append(r.Charges, ChargeSuggestion{
			Code:   ChargeKeyLoss,
			Title:  "钥匙/门禁缺失补办费",
			Detail: fmt.Sprintf("验房结论 keys_status=%s, 补办费 %.2f 元(需房东出示凭证)", s.KeysStatus, s.KeyReplacementFee),
			Basis:  "退租验房钥匙结论 + 入住交割钥匙清单对比",
			Amount: cap0(s.KeyReplacementFee),
		})
	}
	if s.UtilityStatus == "UNPAID" && s.UtilityBilledTotal > 0 && !s.UtilityReserved {
		r.Charges = append(r.Charges, ChargeSuggestion{
			Code:   ChargeUtilityArrears,
			Title:  "水电煤欠费代缴",
			Detail: fmt.Sprintf("入住与退租读数差 × 单价合计 %.2f 元, 验房标记未结清", s.UtilityBilledTotal),
			Basis:  "入住/退租水电煤读数与单价 + 缴费凭证",
			Amount: cap0(s.UtilityBilledTotal),
		})
	}
	if s.CleaningStatus == "NEEDS_CLEANING" {
		r.Charges = append(r.Charges, ChargeSuggestion{
			Code:   ChargeCleaning,
			Title:  "退租清洁费",
			Detail: fmt.Sprintf("验房结论 cleaning_status=NEEDS_CLEANING, 清洁报价 %.2f 元(需票据)", s.CleaningFee),
			Basis:  "退租验房清洁情况照片 + 清洁公司报价/票据",
			Amount: cap0(s.CleaningFee),
		})
		if s.CleaningFee <= 0 {
			r.Notes = append(r.Notes, "清洁费主张未填写金额, 需补充报价凭证后才能扣款")
		}
	}

	// 所有可扣款合计不得超过押金(押金信息缺失时不截断, 由上层处理)。
	preCapTotal := r.DeductibleTotal()
	if s.DepositAmount > 0 {
		r.capByDeposit(s.DepositAmount)
	}

	// 一致性提示
	if preCapTotal > s.DepositAmount && s.DepositAmount > 0 {
		r.Notes = append(r.Notes, "扣款合计超过押金, 已按押金总额截断; 超出部分房东需另行主张")
	}
	return r
}

// surfaceRules 处理墙面/地板: 入住交割基线 + 维修责任 + 验房结论。
func surfaceRules(s Snapshot, cat, status, dmgCode, wearCode, label string, fee float64) []Suggestion {
	switch status {
	case "GOOD":
		return nil
	case "NORMAL_WEAR":
		return []Suggestion{{
			Code:       wearCode,
			Title:      label + "属于自然磨损(建议不扣款)",
			Detail:     "验房结论为 NORMAL_WEAR, 与正常居住损耗相符",
			Basis:      "入住交割照片/清单基线对比 + 合同自然磨损条款",
			Amount:     0,
			Deductible: false,
		}}
	case "DAMAGED":
		// 1) 入住交割时已损坏/已有磨损 -> 非租客责任; 入住时 GOOD/NEW 的新损不能免责。
		if pre := findInventory(s.Inventory, cat); pre == "DAMAGED" || pre == "WORN" {
			return []Suggestion{{
				Code:       CodePreExistingDamage,
				Title:      label + "损坏在入住交割时已存在(建议不扣款)",
				Detail:     fmt.Sprintf("入住清单[%s]基线状态=%s, 退租损坏不能归责于租客", pre, pre),
				Basis:      "入住交割清单与照片(checkin_state=" + pre + ")",
				Amount:     0,
				Deductible: false,
			}}
		}
		// 2) 维修记录决定责任
		if rp := latestRepair(s.Repairs, cat); rp != nil {
			switch rp.Status {
			case "LANDLORD_RESPONSIBLE":
				return []Suggestion{{
					Code:       wearCode,
					Title:      label + "损坏经报修判定为房东责任(建议不扣款)",
					Detail:     "维修工单 " + rp.ID + " 责任方=房东",
					Basis:      "维修责任确认记录(repair=" + rp.ID + ", LANDLORD_RESPONSIBLE)",
					Amount:     0,
					Deductible: false,
				}}
			case "TENANT_RESPONSIBLE":
				amt := fee
				if rp.TenantShare > 0 {
					amt = rp.TenantShare
				}
				return []Suggestion{{
					Code:           dmgCode,
					Title:          label + "损坏扣款(建议)",
					Detail:         fmt.Sprintf("维修工单 %s 判定租客责任, 费用 %.2f 元", rp.ID, amt),
					Basis:          "维修工单 " + rp.ID + "(TENANT_RESPONSIBLE) + 退租验房照片",
					LinkedRepairID: rp.ID,
					Amount:         cap0(amt),
					Deductible:     true,
				}}
			case "SHARED":
				amt := fee / 2
				if rp.CostAmount > 0 {
					amt = rp.CostAmount / 2
				}
				return []Suggestion{{
					Code:           dmgCode,
					Title:          label + "损坏责任共担(建议承担 50%)",
					Detail:         fmt.Sprintf("维修工单 %s 判定责任共担, 建议金额 %.2f 元", rp.ID, amt),
					Basis:          "维修工单 " + rp.ID + "(SHARED) + 验房照片",
					LinkedRepairID: rp.ID,
					Amount:         cap0(amt),
					Deductible:     true,
				}}
			default:
				// 报修过但责任未定论
				return []Suggestion{{
					Code:           dmgCode,
					Title:          label + "损坏扣款建议(维修责任未定论, 需先确认)",
					Detail:         fmt.Sprintf("维修工单 %s 状态=%s, 责任不清; 房东报价 %.2f 元", rp.ID, rp.Status, fee),
					Basis:          "退租验房照片 + 未闭合维修工单 " + rp.ID,
					LinkedRepairID: rp.ID,
					Amount:         cap0(fee),
					Deductible:     true,
				}}
			}
		}
		// 3) 无前置记录: 给房东报价建议, 但注明租客可就自然磨损申诉
		return []Suggestion{{
			Code:       dmgCode,
			Title:      label + "损坏扣款(建议, 租客可就自然磨损申诉)",
			Detail:     fmt.Sprintf("验房标记 DAMAGED, 房东主张修复费 %.2f 元; 无入住前损坏记录", fee),
			Basis:      "退租验房照片 + 入住交割基线对比(基线完好) + 修复报价",
			Amount:     cap0(fee),
			Deductible: true,
		}}
	}
	return nil
}

func applianceRules(s Snapshot) []Suggestion {
	switch s.ApplianceStatus {
	case "GOOD", "NORMAL_WEAR":
		if s.ApplianceStatus == "NORMAL_WEAR" {
			return []Suggestion{{
				Code:       CodeWallNormalWear, // 复用零费用性质, 但 title 指明家电
				Title:      "家电属于自然老化(建议不扣款)",
				Detail:     "验房结论 appliance_status=NORMAL_WEAR",
				Basis:      "入住家电清单 + 退租验房运行状态",
				Amount:     0,
				Deductible: false,
			}}
		}
		return nil
	case "DAMAGED", "MISSING":
		label := "家电损坏"
		if s.ApplianceStatus == "MISSING" {
			label = "家电灭失"
		}
		pre := findInventory(s.Inventory, "APPLIANCE")
		if pre == "MISSING" {
			return []Suggestion{{
				Code:       CodePreExistingDamage,
				Title:      "家电在入住交割时即缺失(建议不扣款)",
				Basis:      "入住交割家电清单(checkin_state=MISSING)",
				Amount:     0,
				Deductible: false,
			}}
		}
		if rp := latestRepair(s.Repairs, "APPLIANCE"); rp != nil {
			switch rp.Status {
			case "LANDLORD_RESPONSIBLE":
				return []Suggestion{{
					Code:       CodeApplianceLandlordLiable,
					Title:      label + ": 维修记录判定房东责任(建议不扣款)",
					Detail:     "维修工单 " + rp.ID + " 责任方=房东",
					Basis:      "维修工单 " + rp.ID + "(LANDLORD_RESPONSIBLE)",
					Amount:     0,
					Deductible: false,
				}}
			case "TENANT_RESPONSIBLE":
				amt := s.ApplianceFee
				if rp.TenantShare > 0 {
					amt = rp.TenantShare
				}
				return []Suggestion{{
					Code:           CodeApplianceTenantLiable,
					Title:          label + "扣款(建议)",
					Detail:         fmt.Sprintf("维修工单 %s 判定租客责任, 费用 %.2f 元", rp.ID, amt),
					Basis:          "维修工单 " + rp.ID + "(TENANT_RESPONSIBLE) + 验房结论",
					LinkedRepairID: rp.ID,
					Amount:         cap0(amt),
					Deductible:     true,
				}}
			case "SHARED":
				amt := s.ApplianceFee / 2
				if rp.CostAmount > 0 {
					amt = rp.CostAmount / 2
				}
				return []Suggestion{{
					Code:           CodeApplianceTenantLiable,
					Title:          label + "责任共担(建议承担 50%)",
					Detail:         fmt.Sprintf("维修工单 %s 判定共担, 建议 %.2f 元", rp.ID, amt),
					Basis:          "维修工单 " + rp.ID + "(SHARED)",
					LinkedRepairID: rp.ID,
					Amount:         cap0(amt),
					Deductible:     true,
				}}
			default:
				return []Suggestion{{
					Code:           CodeApplianceUnclear,
					Title:          label + "但维修责任不清(必须先澄清责任)",
					Detail:         fmt.Sprintf("维修工单 %s 状态=%s 未定论; 暂缓扣款, 建议双方补证或平台裁决", rp.ID, rp.Status),
					Basis:          "未闭合维修工单 " + rp.ID + " + 入住家电清单 + 验房结论",
					LinkedRepairID: rp.ID,
					Amount:         cap0(s.ApplianceFee),
					Deductible:     true,
				}}
			}
		}
		// 没有任何维修记录, 责任完全不清
		return []Suggestion{{
			Code:       CodeApplianceUnclear,
			Title:      label + "但缺少维修与责任记录(必须先澄清责任)",
			Detail:     fmt.Sprintf("入住期间无报修记录, 无法区分人为损坏与自然老化; 房东报价 %.2f 元仅供参考", s.ApplianceFee),
			Basis:      "入住家电清单 + 退租验房结论; 缺少维修工单, 举证责任待分配",
			Amount:     cap0(s.ApplianceFee),
			Deductible: true,
		}}
	}
	return nil
}

// DeductibleTotal 汇总所有可扣款建议金额。
func (r Report) DeductibleTotal() float64 {
	t := 0.0
	for _, f := range r.Facts {
		if f.Deductible {
			t += f.Amount
		}
	}
	for _, c := range r.Charges {
		t += c.Amount
	}
	return round2(t)
}

func (r *Report) capByDeposit(deposit float64) {
	remaining := deposit
	cut := func(amt float64) float64 {
		if amt <= remaining {
			remaining -= amt
			return amt
		}
		a := remaining
		remaining = 0
		return a
	}
	for i := range r.Facts {
		if r.Facts[i].Deductible && r.Facts[i].Amount > 0 {
			r.Facts[i].Amount = round2(cut(r.Facts[i].Amount))
		}
	}
	for i := range r.Charges {
		if r.Charges[i].Amount > 0 {
			r.Charges[i].Amount = round2(cut(r.Charges[i].Amount))
		}
	}
}

func findInventory(items []InventoryView, category string) string {
	for _, it := range items {
		if it.Category == category {
			return it.CheckinState
		}
	}
	return ""
}

func latestRepair(reps []RepairView, category string) *RepairView {
	for i := range reps {
		if reps[i].Category == category {
			return &reps[i]
		}
	}
	return nil
}

func cap0(v float64) float64 {
	v = round2(v)
	if v < 0 {
		return 0
	}
	return v
}

func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}
