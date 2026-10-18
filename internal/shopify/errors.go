package shopify

import (
	"bytes"
	"encoding/json"
	"strings"
)

// GraphQLError is one entry in a response's top-level "errors" list.
type GraphQLError struct {
	Message string
	Code    string // extensions.code, such as "THROTTLED"; "" if Shopify sent none
}

// GraphQLErrors is the error Query returns when Shopify answers with
// top-level GraphQL errors. Get it with errors.As.
type GraphQLErrors []GraphQLError

func (e GraphQLErrors) Error() string {
	msgs := make([]string, len(e))
	for i, ge := range e {
		msgs[i] = ge.Message
	}
	return "shopify: graphql: " + strings.Join(msgs, "; ")
}

// has reports whether any of the errors has the given code.
func (e GraphQLErrors) has(code string) bool {
	for _, ge := range e {
		if ge.Code == code {
			return true
		}
	}
	return false
}

// UserError is one entry in a mutation's "userErrors" list: Shopify ran the
// mutation but refused the input.
type UserError struct {
	Field   []string `json:"field"`
	Message string   `json:"message"`
	Code    string   `json:"code"` // set only if the query asks for it
}

// UserErrors is the error Query returns when a mutation reports user
// errors. Get it with errors.As.
type UserErrors []UserError

func (e UserErrors) Error() string {
	msgs := make([]string, len(e))
	for i, ue := range e {
		msgs[i] = ue.Message
	}
	return "shopify: user errors: " + strings.Join(msgs, "; ")
}

// findUserErrors returns the user errors in a response's data. A mutation's
// result sits under its own name at the top of data, and holds its user
// errors in a "userErrors" list, so only the top level is searched.
func findUserErrors(data json.RawMessage) UserErrors {
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil {
		return nil
	}
	var found UserErrors
	for _, field := range fields {
		// Only objects can hold userErrors; a field may also be a list or null.
		if !bytes.HasPrefix(bytes.TrimSpace(field), []byte("{")) {
			continue
		}
		var payload struct {
			UserErrors UserErrors `json:"userErrors"`
		}
		if json.Unmarshal(field, &payload) == nil {
			found = append(found, payload.UserErrors...)
		}
	}
	return found
}
