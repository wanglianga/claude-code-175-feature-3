package app

import "deposit-escrow/internal/domain"

// 本文件是水电尾款"账单 × 搬离当天表计照片"复核的纯函数逻辑(无 IO, 便于单测)。
//
// 复核铁律: 租客提交了搬离当天水表/电表照片时, 必须把照片读数与供应商账单读数放在一起比对,
// 不能只按账单金额扣款。账单延迟、读数无法对应、双方对费用分摊有争议时, 不得最终结算。

// UtilityReviewItem 是单个品类(水/电/燃气)的复核输入。
type UtilityReviewItem struct {
	Kind string
	// 入住读数(用量区间起点)与租客搬离时平台掌握的最近读数
	CheckinReading float64
	LastReading    float64
	// 供应商账单
	HasBill      bool
	BillReading  float64
	BilledAmount float64
	// 租客搬离当天表计照片推算
	HasPhoto     bool
	PhotoReading float64
	PhotoAmount  float64
	// 读数比对容差(表计最小分度/账期跨日误差)
	Tolerance float64
	// 该品类是否存在未闭合的费用分摊争议
	SplitDisputed bool
}

// UtilityReviewDecision 是纯复核结果。
type UtilityReviewDecision struct {
	// 每个品类的定论费用与标志(OK/BILL_DELAY/READING_MISMATCH/SPLIT_DISPUTE)
	FinalAmounts map[string]float64
	Flags        map[string]string
	BilledTotal  float64
	PhotoTotal   float64
	FinalTotal   float64
	// Blocker 为空表示可以自动最终结算; 否则资金必须继续挂账并进入对应处理流程。
	Blocker string // "" | BILL_DELAY | READING_MISMATCH | SPLIT_DISPUTE
	// Decision 仅在 Blocker=="" 时有值: BILL=按账单(照片缺失或与账单一致)
	Decision string
	Notes    []string
}

// ReviewUtilityItems 把供应商账单与搬离照片读数放在一起复核。
//
// 判定顺序:
//  1. 任一分项存在未闭合分摊争议 -> SPLIT_DISPUTE, 全部挂账;
//  2. 账单缺失 -> BILL_DELAY(最后一期账单尚未生成);
//  3. 账单读数低于搬离最近读数, 或与照片读数差超过容差 -> READING_MISMATCH(读数无法对应);
//  4. 其余按账单实际金额定论; 有照片且一致时在备注中留痕, 证明并非"只按账单扣款"。
func ReviewUtilityItems(items []UtilityReviewItem) UtilityReviewDecision {
	d := UtilityReviewDecision{
		FinalAmounts: map[string]float64{},
		Flags:        map[string]string{},
	}
	priority := func(b string) {
		// SPLIT_DISPUTE > READING_MISMATCH > BILL_DELAY
		rank := map[string]int{"": 0, "BILL_DELAY": 1, "READING_MISMATCH": 2, "SPLIT_DISPUTE": 3}
		if rank[b] > rank[d.Blocker] {
			d.Blocker = b
		}
	}
	photosUsed := 0
	for _, it := range items {
		d.BilledTotal = round2(d.BilledTotal + it.BilledAmount)
		d.PhotoTotal = round2(d.PhotoTotal + it.PhotoAmount)

		switch {
		case it.SplitDisputed:
			d.Flags[it.Kind] = domain.UtilityFlagSplitDispute
			priority(domain.UtilityFlagSplitDispute)
			d.Notes = append(d.Notes, it.Kind+": 双方对费用分摊存在争议, 转人工复核")
		case !it.HasBill:
			d.Flags[it.Kind] = domain.UtilityFlagBillDelay
			priority(domain.UtilityFlagBillDelay)
			d.Notes = append(d.Notes, it.Kind+": 最后一期供应商账单尚未生成, 预留款保持待处理")
		case billReadingDoesNotCorrespond(it):
			d.Flags[it.Kind] = domain.UtilityFlagReadingMismatch
			priority(domain.UtilityFlagReadingMismatch)
			d.Notes = append(d.Notes, it.Kind+
				": 账单读数与搬离读数/照片读数无法对应, 转人工复核, 不得只按账单扣款")
		default:
			d.Flags[it.Kind] = domain.UtilityFlagOK
			d.FinalAmounts[it.Kind] = round2(it.BilledAmount)
			d.FinalTotal = round2(d.FinalTotal + it.BilledAmount)
			if it.HasPhoto {
				photosUsed++
				d.Notes = append(d.Notes, it.Kind+
					": 搬离当天照片读数与供应商账单读数一致(容差内), 按账单实际金额结算")
			} else {
				d.Notes = append(d.Notes, it.Kind+": 无搬离照片, 账单读数不低于最近读数, 按账单实际金额结算")
			}
		}
	}
	if d.Blocker == "" {
		if photosUsed == len(items) && len(items) > 0 {
			d.Decision = "PHOTO" // 全部品类都有照片且与账单吻合
		} else {
			d.Decision = "BILL"
		}
	}
	return d
}

// billReadingDoesNotCorrespond 判断账单读数能否与搬离记录对应:
//   - 账单读数不得低于平台掌握的最近读数(扣除容差), 否则用量区间无法对应;
//   - 租客提供了搬离当天照片时, 账单读数必须与照片读数在容差内一致。
func billReadingDoesNotCorrespond(it UtilityReviewItem) bool {
	tol := it.Tolerance
	if it.BillReading+tol < it.LastReading {
		return true
	}
	if it.HasPhoto && absFloat(it.BillReading-it.PhotoReading) > tol {
		return true
	}
	return false
}

// EstimateUtilityReserve 依据最近读数与历史月均用量估算每个品类应预留的费用。
// 投影一个账期(月): 历史月均用量 × 单价。纯函数。
func EstimateUtilityReserve(checkin, last float64, unitPrice float64, tenureDays int) float64 {
	months := float64(tenureDays) / 30.0
	if months < 1 {
		months = 1
	}
	avgMonthly := (last - checkin) / months
	if avgMonthly < 0 {
		avgMonthly = 0
	}
	return round2(avgMonthly * unitPrice)
}

func absFloat(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
