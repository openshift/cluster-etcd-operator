package internal

import (
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

type ByRevision []*corev1.Pod

func (s ByRevision) Len() int {
	return len(s)
}
func (s ByRevision) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}
func (s ByRevision) Less(i, j int) bool {
	jRevision, err := GetRevisionOfPod(s[j])
	if err != nil {
		return true
	}
	iRevision, err := GetRevisionOfPod(s[i])
	if err != nil {
		return false
	}
	return iRevision < jRevision
}

func GetRevisionOfPod(pod *corev1.Pod) (int, error) {
	tokens := strings.Split(pod.Name, "-")
	if len(tokens) < 2 {
		return -1, fmt.Errorf("missing revision: %v", pod.Name)
	}
	revision, err := strconv.ParseInt(tokens[1], 10, 32)
	if err != nil {
		return -1, fmt.Errorf("bad revision for %v: %w", pod.Name, err)
	}
	return int(revision), nil
}
