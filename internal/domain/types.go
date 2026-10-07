// Package domain 定义押金托管争议服务的领域模型与状态机。
//
// 核心不变量(由 DB 约束 + 应用层事务共同保证):
//  1. 资金状态随证据/争议状态联动: 扣款提议 -> disputed_hold; 定论 -> hard_hold; 撤销 -> 解冻。
//  2. 存在未闭合争议时, 结算不得确认、押金不得释放 (settlement_guard 触发器)。
//  3. 任何一方都不能单方面扣款: 租客发起申诉即把 hard_hold 退回 disputed_hold 并保持争议打开。
package domain

import "time"

// 合同状态
const (
	StatusDraft             = "DRAFT"
	StatusSigned            = "SIGNED"
	StatusEscrowed          = "ESCROWED"
	StatusLiving            = "LIVING"
	StatusCheckoutPending   = "CHECKOUT_PENDING"
	StatusInspecting        = "INSPECTING"
	StatusDeductionProposed = "DEDUCTION_PROPOSED"
	StatusDisputing         = "DISPUTING"
	StatusSettled           = "SETTLED"
	StatusReleased          = "RELEASED"
	StatusClosed            = "CLOSED"
	StatusEarlyTerminated   = "EARLY_TERMINATED"
)

// 扣款项/费用项状态
const (
	ItemProposed   = "PROPOSED"
	ItemAccepted   = "ACCEPTED"
	ItemRejected   = "REJECTED"
	ItemContested  = "CONTESTED"
	ItemUpheld     = "UPHELD"
	ItemOverturned = "OVERTURNED"
)

// 争议状态
const (
	DisputeOpen            = "OPEN"
	DisputeEvidencePending = "EVIDENCE_PENDING"
	DisputeUnderReview     = "UNDER_REVIEW"
	DisputeResolvedAgree   = "RESOLVED_AGREEMENT"
	DisputeResolvedAdjud   = "RESOLVED_ADJUDICATION"
	DisputeWithdrawn       = "WITHDRAWN"
)

func OpenDisputeStatuses() []string {
	return []string{DisputeOpen, DisputeEvidencePending, DisputeUnderReview}
}

// 水电尾款预留状态
const (
	UtilityReserveReserved      = "RESERVED"       // 已预留, 等待最后一期供应商账单
	UtilityReserveReviewPending = "REVIEW_PENDING" // 待处理(账单延迟等, 暂挂)
	UtilityReserveManualReview  = "MANUAL_REVIEW"  // 读数无法对应/分摊争议, 人工复核
	UtilityReserveFinalized     = "FINALIZED"      // 已最终结算(多退少补定论)
)

// UtilityFinalDisputeReason 是水电尾款分摊/读数争议的原因码。
const UtilityFinalDisputeReason = "UTILITY_FINAL_SETTLEMENT"

// 复核分项标志
const (
	UtilityFlagOK              = "OK"
	UtilityFlagBillDelay       = "BILL_DELAY"
	UtilityFlagReadingMismatch = "READING_MISMATCH"
	UtilityFlagSplitDispute    = "SPLIT_DISPUTE"
)

type Contract struct {
	ID                string    `json:"id"`
	LeaseNo           string    `json:"lease_no"`
	PropertyID        string    `json:"property_id"`
	LandlordID        string    `json:"landlord_id"`
	TenantID          string    `json:"tenant_id"`
	AgentID           *string   `json:"agent_id,omitempty"`
	StartDate         string    `json:"start_date"`
	EndDate           string    `json:"end_date"`
	RentAmount        float64   `json:"rent_amount"`
	DepositAmount     float64   `json:"deposit_amount"`
	WearClause        string    `json:"wear_clause,omitempty"`
	NoticeDays        int       `json:"notice_days"`
	InspectionDueDays int       `json:"inspection_due_days"`
	PaymentDueDays    int       `json:"payment_due_days"`
	Status            string    `json:"status"`
	CreatedAt         time.Time `json:"created_at"`
}

type Escrow struct {
	ID            string    `json:"id"`
	ContractID    string    `json:"contract_id"`
	Currency      string    `json:"currency"`
	Balance       float64   `json:"balance"`
	HardHold      float64   `json:"hard_hold"`
	DisputedHold  float64   `json:"disputed_hold"`
	UtilityHold   float64   `json:"utility_hold"`
	ReleasedTotal float64   `json:"released_total"`
	Version       int64     `json:"version"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Available 返回当前可释放给租客的金额(未被任何预留占用)。
func (e Escrow) Available() float64 {
	return e.Balance - e.HardHold - e.DisputedHold - e.UtilityHold
}

type InventoryItem struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	Category      string  `json:"category"`
	BrandModel    string  `json:"brand_model,omitempty"`
	CheckinState  string  `json:"checkin_state"`
	CheckinNote   string  `json:"checkin_note,omitempty"`
	CheckoutState *string `json:"checkout_state,omitempty"`
	CheckoutNote  string  `json:"checkout_note,omitempty"`
}

type Repair struct {
	ID           string     `json:"id"`
	ContractID   string     `json:"contract_id"`
	ItemName     string     `json:"item_name,omitempty"`
	Title        string     `json:"title"`
	Description  string     `json:"description"`
	Category     string     `json:"category"`
	Status       string     `json:"status"`
	CostAmount   float64    `json:"cost_amount"`
	TenantShare  float64    `json:"tenant_share"`
	DecisionNote string     `json:"decision_note,omitempty"`
	SubmittedBy  string     `json:"submitted_by"`
	DecidedBy    *string    `json:"decided_by,omitempty"`
	OccurredAt   time.Time  `json:"occurred_at"`
	DecidedAt    *time.Time `json:"decided_at,omitempty"`
}

// InspectionInput 是验房录入的结构化结论。
type InspectionInput struct {
	WallStatus      string `json:"wall_status"`
	FloorStatus     string `json:"floor_status"`
	ApplianceStatus string `json:"appliance_status"`
	KeysStatus      string `json:"keys_status"`
	UtilityStatus   string `json:"utility_status"`
	CleaningStatus  string `json:"cleaning_status"`
	Summary         string `json:"summary"`
}

type Finding struct {
	ID             string   `json:"id"`
	InspectionID   *string  `json:"inspection_id,omitempty"`
	Code           string   `json:"code"`
	Title          string   `json:"title"`
	Detail         string   `json:"detail,omitempty"`
	Basis          string   `json:"basis"`
	EvidenceIDs    []string `json:"evidence_ids"`
	LinkedRepairID *string  `json:"linked_repair_id,omitempty"`
	ProposedAmount float64  `json:"proposed_amount"`
	Status         string   `json:"status"`
	Source         string   `json:"source"`
}

// Actor 是当前操作者(由认证层注入)。
type Actor struct {
	ID   string `json:"id"`
	Role string `json:"role"`
	Name string `json:"name"`
}

type Dispute struct {
	ID              string     `json:"id"`
	ContractID      string     `json:"contract_id"`
	Reason          string     `json:"reason"`
	Subject         string     `json:"subject"`
	ClaimedAmount   float64    `json:"claimed_amount"`
	FindingID       *string    `json:"finding_id,omitempty"`
	ChargeID        *string    `json:"charge_id,omitempty"`
	OpenedBy        string     `json:"opened_by"`
	Status          string     `json:"status"`
	AwardToLandlord float64    `json:"award_to_landlord"`
	AwardToTenant   float64    `json:"award_to_tenant"`
	ResolutionNote  string     `json:"resolution_note,omitempty"`
	Deadline        *time.Time `json:"deadline,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	ResolvedAt      *time.Time `json:"resolved_at,omitempty"`
}
