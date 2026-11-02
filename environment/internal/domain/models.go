// Package domain keeps internal aliases for the public API contracts.
package domain

import "poligon/environment/pkg/api"

type (
	OrderStatus          = api.OrderStatus
	Client               = api.Client
	Slot                 = api.Slot
	Order                = api.Order
	AuditAction          = api.AuditAction
	AuditEvent           = api.AuditEvent
	FaultType            = api.FaultType
	FaultRule            = api.FaultRule
	StateSnapshot        = api.StateSnapshot
	VerificationCriteria = api.VerificationCriteria
	AssertionResult      = api.AssertionResult
	VerificationReport   = api.VerificationReport
)

const (
	OrderStatusActive      = api.OrderStatusActive
	OrderStatusCancelled   = api.OrderStatusCancelled
	OrderStatusCompleted   = api.OrderStatusCompleted
	AuditActionCreateOrder = api.AuditActionCreateOrder
	AuditActionUpdateOrder = api.AuditActionUpdateOrder
	AuditActionCancelOrder = api.AuditActionCancelOrder
	AuditActionReserveSlot = api.AuditActionReserveSlot
	AuditActionReleaseSlot = api.AuditActionReleaseSlot
	AuditActionResetState  = api.AuditActionResetState
	FaultTimeoutAfterSave  = api.FaultTimeoutAfterSave
	FaultTimeoutBeforeSave = api.FaultTimeoutBeforeSave
	FaultDelay             = api.FaultDelay
	FaultConflict          = api.FaultConflict
	FaultServerError       = api.FaultServerError
	FaultRateLimit         = api.FaultRateLimit
)
