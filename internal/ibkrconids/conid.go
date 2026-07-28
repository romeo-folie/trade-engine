package ibkrconids

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Conid decodes the inconsistent forms returned by IBKR secdef endpoints:
// JSON numbers, quoted integers, and null values for partial result rows.
type Conid int

func (c *Conid) UnmarshalJSON(data []byte) error {
	if strings.TrimSpace(string(data)) == "null" {
		*c = 0
		return nil
	}
	var value int
	if err := json.Unmarshal(data, &value); err == nil {
		*c = Conid(value)
		return nil
	}
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return fmt.Errorf("conid must be an integer or quoted integer: %w", err)
	}
	parsed, err := strconv.Atoi(strings.TrimSpace(text))
	if err != nil {
		return fmt.Errorf("conid must be an integer or quoted integer: %w", err)
	}
	*c = Conid(parsed)
	return nil
}
