package database

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// ConnStringError is the error to log or return when the connection string
// held by setting (e.g. "SCOUT_DATABASE_URL") cannot be parsed by pgx. Its
// text names the setting and, when it is a known fixed text, the reason; it
// never contains the connection string, its user, password or query string.
//
// err is deliberately not wrapped: pgconn's ParseConfigError redacts the
// password in its own text, but the url.Error it may wrap quotes the whole
// URL, password included.
func ConnStringError(setting string, err error) error {
	reason := connStringReason(err)
	if reason == "" {
		return fmt.Errorf("%s could not be parsed", setting)
	}
	return fmt.Errorf("%s could not be parsed: %s", setting, reason)
}

// connStringReason returns a reason for a pgx, pgxpool or pgconn ParseConfig
// error that cannot contain the connection string, or "" when none is known.
func connStringReason(err error) string {
	var pe *pgconn.ParseConfigError
	if errors.As(err, &pe) {
		return parseConfigReason(pe)
	}
	return optionReason(err)
}

// parseConfigReason returns pgconn's message for pe (e.g. "invalid port"),
// followed by its cause when that is a fixed text, or "" when the message is
// not one of pgconn's known ones.
func parseConfigReason(pe *pgconn.ParseConfigError) string {
	msg := parseConfigMessage(pe)
	if !slices.Contains(pgconnMessages, msg) {
		return ""
	}
	if cause := causeReason(pe.Unwrap()); cause != "" {
		return msg + ": " + cause
	}
	return msg
}

// pgconnMessages are pgconn's ParseConfigError messages (pgx v5), up to the
// first colon. Only these are shown.
var pgconnMessages = []string{
	"failed to parse as URL",
	"failed to parse as keyword/value",
	"failed to read service",
	"invalid connect_timeout",
	"invalid port",
	"failed to configure TLS",
	"unknown target_session_attrs value",
}

// emptyConnStringPrefix starts the text of a pgconn ParseConfigError whose
// ConnString is empty (pgconn puts the string between two backquotes).
const emptyConnStringPrefix = "cannot parse ``: "

// parseConfigMessage returns pe's message without the connection string, the
// cause, or anything after a colon (such as the target_session_attrs value).
// pgconn keeps the message unexported, so it is cut from the text of a copy
// whose ConnString is empty: emptyConnStringPrefix, then "<message> (<cause>)".
func parseConfigMessage(pe *pgconn.ParseConfigError) string {
	bare := *pe
	bare.ConnString = ""
	text, ok := strings.CutPrefix(bare.Error(), emptyConnStringPrefix)
	if !ok {
		return ""
	}
	if cause := pe.Unwrap(); cause != nil {
		if text, ok = strings.CutSuffix(text, " ("+cause.Error()+")"); !ok {
			return ""
		}
	}
	msg, _, _ := strings.Cut(text, ":")
	return msg
}

// fixedCauses are the causes pgconn returns as fixed texts, without any part
// of the connection string.
var fixedCauses = []string{
	"sslmode is invalid",
	"invalid keyword/value",
	"invalid backslash",
	"unterminated quoted string in connection info string",
	`both "sslcert" and "sslkey" are required`,
	"outside range",
}

// causeReason describes cause when that is safe: a fixed pgconn text, or a
// number that did not parse (without the number). Anything else, such as a
// url.Error, which quotes the whole URL, gives "".
func causeReason(cause error) string {
	switch {
	case cause == nil:
		return ""
	case errors.Is(cause, strconv.ErrSyntax):
		return "not a number"
	case errors.Is(cause, strconv.ErrRange):
		return "number out of range"
	case slices.Contains(fixedCauses, cause.Error()):
		return cause.Error()
	}
	return ""
}

// connStringOptions are the query-string options that pgx and pgxpool parse
// after pgconn, with an error of their own that names the option.
var connStringOptions = []string{
	"statement_cache_capacity",
	"description_cache_capacity",
	"default_query_exec_mode",
	"pool_max_conns",
	"pool_min_conns",
	"pool_max_conn_lifetime",
	"pool_max_conn_idle_time",
	"pool_health_check_period",
	"pool_max_conn_lifetime_jitter",
}

// optionReason returns "invalid <option>" when err is pgx's or pgxpool's
// error for one of connStringOptions, without the option's value; "" for
// any other error.
func optionReason(err error) string {
	if err == nil {
		return ""
	}
	text := err.Error()
	for _, name := range connStringOptions {
		if strings.Contains(text, name+":") || strings.Contains(text, name+" ") {
			return "invalid " + name
		}
	}
	return ""
}
