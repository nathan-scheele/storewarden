package tools

import (
	"fmt"
	"strings"
)

// checkID returns an error unless id is a Shopify ID of the given kind, such
// as gid://shopify/Product/123. field names the input the ID came from.
func checkID(field, kind, id string) error {
	prefix := "gid://shopify/" + kind + "/"
	number, ok := strings.CutPrefix(id, prefix)
	// Trimming every digit leaves "" only if number is all digits.
	if !ok || number == "" || strings.Trim(number, "0123456789") != "" {
		return fmt.Errorf("%s must look like %s123, not %q", field, prefix, id)
	}
	return nil
}
