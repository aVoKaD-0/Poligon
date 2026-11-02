package api

// CreateOrderRequest is the payload to book an appointment.
type CreateOrderRequest struct {
	ClientID       string `json:"client_id"`
	ServiceType    string `json:"service_type"`
	Address        string `json:"address"`
	Date           string `json:"date"`
	TimeWindow     string `json:"time_window"`
	SlotID         string `json:"slot_id,omitempty"`
	Notes          string `json:"notes,omitempty"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// UpdateOrderRequest is the payload to reschedule or update address/details.
type UpdateOrderRequest struct {
	Address    *string `json:"address,omitempty"`
	Date       *string `json:"date,omitempty"`
	TimeWindow *string `json:"time_window,omitempty"`
	SlotID     *string `json:"slot_id,omitempty"`
	Notes      *string `json:"notes,omitempty"`
}
