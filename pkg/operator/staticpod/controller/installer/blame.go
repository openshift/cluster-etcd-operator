package installer

import (
	"fmt"
	"strings"
)

// reasonWithBlame inspects the reason given by the installer pod, which usually include the last log line from the installer pod
// and try to point a finger on component that likely caused its failure to provide better operator message.
func reasonWithBlame(originalReason string) string {
	switch {
	case strings.Contains(originalReason, "no route to host"):
		return fmt.Sprintf("%s (most likely node networking is misbehaving)", originalReason)
	default:
		return originalReason
	}
}
