package doer

// HertzDoerError is a structured error type for HertzDoer operations.
// Kind categorizes the error for metrics/alerting:
// "validation", "security", "timeout", "recovery", "http_client", "status",
// "circuit_open", "backpressure".
type HertzDoerError struct {
	Cause   error
	Kind    string
	Message string
}

func (e *HertzDoerError) Error() string {
	if e.Cause != nil {
		return "hertz_doer: " + e.Kind + ": " + e.Message + ": " + e.Cause.Error()
	}
	return "hertz_doer: " + e.Kind + ": " + e.Message
}

func (e *HertzDoerError) Unwrap() error {
	return e.Cause
}
