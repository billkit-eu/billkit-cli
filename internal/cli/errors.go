package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/billkit-eu/billkit-cli/internal/api"
)

// cliErrorType is the `type` of an error the CLI raised itself rather than
// relayed from the API. The API's own types (`invalid_request_error`,
// `authentication_error`, ...) never collide with it.
const cliErrorType = "cli_error"

// Codes for errors the CLI raised itself, one per exit status.
const (
	codeUsage   = "invalid_usage"
	codeAuth    = "authentication_required"
	codeFailure = "command_failed"
)

// errorEnvelope is what --json writes to stderr when a command fails. Its
// `error` object is the API's own envelope, field for field, so a script
// that already reads API errors reads these too, plus two fields the API
// never sends: `http_status` (absent when no HTTP answer was involved) and
// the process `exit_code`.
type errorEnvelope struct {
	Error    errorBody `json:"error"`
	ExitCode int       `json:"exit_code"`
}

type errorBody struct {
	Type       string `json:"type"`
	Code       string `json:"code"`
	Message    string `json:"message"`
	Param      string `json:"param,omitempty"`
	Reason     string `json:"reason,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

// writeError prints a failed command's error to w, which is stderr.
//
// The text form is the stable contract scripts may match on:
//
//	Error: <message>
//
// where an API error's message is `<code> (HTTP <status>): <message>`,
// followed by ` [reason: <reason>, param: <param>]` when the envelope
// carries either. With --json it is one errorEnvelope object on one line.
func writeError(w io.Writer, err error, asJSON bool) {
	if !asJSON {
		fmt.Fprintln(w, "Error: "+err.Error())
		return
	}
	raw, mErr := json.Marshal(envelopeFor(err))
	if mErr != nil {
		fmt.Fprintln(w, "Error: "+err.Error())
		return
	}
	fmt.Fprintln(w, string(raw))
}

// envelopeFor maps an error onto the JSON envelope.
func envelopeFor(err error) errorEnvelope {
	exit := ExitCode(err)
	var apiErr *api.APIError
	if errors.As(err, &apiErr) {
		body := errorBody{
			Type:       apiErr.Type,
			Code:       apiErr.Code,
			Message:    apiErr.Message,
			Param:      apiErr.Param,
			Reason:     apiErr.Reason,
			HTTPStatus: apiErr.StatusCode,
		}
		if body.Type == "" {
			body.Type = "api_error"
		}
		if body.Message == "" {
			body.Message = err.Error()
		}
		return errorEnvelope{Error: body, ExitCode: exit}
	}
	var stream *streamStatusError
	if errors.As(err, &stream) {
		parsed := api.ParseError(stream.status, []byte(stream.body))
		body := errorBody{
			Type: parsed.Type, Code: parsed.Code, Message: err.Error(),
			Param: parsed.Param, Reason: parsed.Reason, HTTPStatus: stream.status,
		}
		if body.Type == "" {
			body.Type = "api_error"
		}
		return errorEnvelope{Error: body, ExitCode: exit}
	}
	code := codeFailure
	switch exit {
	case ExitUsage:
		code = codeUsage
	case ExitAuth:
		code = codeAuth
	}
	return errorEnvelope{Error: errorBody{Type: cliErrorType, Code: code, Message: err.Error()}, ExitCode: exit}
}
