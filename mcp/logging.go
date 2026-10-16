package mcp

import "net/http"

/* Logging */

// SetLevelRequest is a request from the client to the server, to enable or
// adjust logging.
type SetLevelRequest struct {
	Request
	Params SetLevelParams `json:"params"`
	Header http.Header    `json:"-"`
}

type SetLevelParams struct {
	// The level of logging that the client wants to receive from the server.
	// The server should send all logs at this level and higher (i.e., more severe) to
	// the client as notifications/logging/message.
	Level LoggingLevel `json:"level"`
}

// LoggingMessageNotification is a notification of a log message passed from
// server to client. If no logging/setLevel request has been sent from the client,
// the server MAY decide which messages to send automatically.
type LoggingMessageNotification struct {
	Notification
	Params LoggingMessageNotificationParams `json:"params"`
}

type LoggingMessageNotificationParams struct {
	// The severity of this log message.
	Level LoggingLevel `json:"level"`
	// An optional name of the logger issuing this message.
	Logger string `json:"logger,omitempty"`
	// The data to be logged, such as a string message or an object. Any JSON
	// serializable type is allowed here.
	Data any `json:"data"`
}

// LoggingLevel represents the severity of a log message.
//
// These map to syslog message severities, as specified in RFC-5424:
// https://datatracker.ietf.org/doc/html/rfc5424#section-6.2.1
type LoggingLevel string

const (
	LoggingLevelDebug     LoggingLevel = "debug"
	LoggingLevelInfo      LoggingLevel = "info"
	LoggingLevelNotice    LoggingLevel = "notice"
	LoggingLevelWarning   LoggingLevel = "warning"
	LoggingLevelError     LoggingLevel = "error"
	LoggingLevelCritical  LoggingLevel = "critical"
	LoggingLevelAlert     LoggingLevel = "alert"
	LoggingLevelEmergency LoggingLevel = "emergency"
)

var levelToInt = map[LoggingLevel]int{
	LoggingLevelDebug:     0,
	LoggingLevelInfo:      1,
	LoggingLevelNotice:    2,
	LoggingLevelWarning:   3,
	LoggingLevelError:     4,
	LoggingLevelCritical:  5,
	LoggingLevelAlert:     6,
	LoggingLevelEmergency: 7,
}

// IsValid reports whether l is one of the eight levels the protocol defines.
func (l LoggingLevel) IsValid() bool {
	_, ok := levelToInt[l]
	return ok
}

func (l LoggingLevel) ShouldSendTo(minLevel LoggingLevel) bool {
	ia, oka := levelToInt[l]
	ib, okb := levelToInt[minLevel]
	if !oka || !okb {
		return false
	}
	return ia >= ib
}
