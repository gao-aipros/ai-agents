package request

var (
	ErrThreadBusy       = &RequestError{Status: 409, Message: "Thread is already processing a request"}
	ErrConcurrencyLimit = &RequestError{Status: 503, Message: "Too many concurrent requests"}
	ErrNoRunningRequest = &RequestError{Status: 404, Message: "No running request for this thread"}
)

// RequestError is an error with an HTTP status code.
type RequestError struct {
	Status  int    `json:"-"`
	Message string `json:"error"`
}

func (e *RequestError) Error() string { return e.Message }
