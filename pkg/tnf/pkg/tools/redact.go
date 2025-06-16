package tools

import (
	"fmt"
	"regexp"
)

const replaceWith = "<REDACTED>"

var (
	// "--password secret"
	passwordDashVerboseRegEx = regexp.MustCompile(`(.* --password )\S*(.*)`)
	// "-p secret"
	passwordDashShortRegEx = regexp.MustCompile(`(.* -p )\S*(.*)`)
	// "password=secret"
	passwordEqualRegEx = regexp.MustCompile(`(.* password=)\S*(.*)`)
	//  "name": "password", "value": "password"
	passwordJSONRegEx = regexp.MustCompile(`(.*"password", "value": ")\S*(".*)`)
	//  name="password" value="password"
	passwordCIBRegEx = regexp.MustCompile(`(.*"password" value=")\S*(".*)`)
)

// RedactPasswords redacts password-like patterns from the input string, replacing them with a placeholder text.
func RedactPasswords(in string) string {
	result := in
	result = passwordDashVerboseRegEx.ReplaceAllString(result, fmt.Sprintf("$1%s$2", replaceWith))
	result = passwordDashShortRegEx.ReplaceAllString(result, fmt.Sprintf("$1%s$2", replaceWith))
	result = passwordEqualRegEx.ReplaceAllString(result, fmt.Sprintf("$1%s$2", replaceWith))
	result = passwordJSONRegEx.ReplaceAllString(result, fmt.Sprintf("$1%s$2", replaceWith))
	result = passwordCIBRegEx.ReplaceAllString(result, fmt.Sprintf("$1%s$2", replaceWith))
	return result
}
