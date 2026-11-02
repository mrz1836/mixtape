package capture

import (
	"fmt"
	"strings"
)

// ParseMethod parses a capture method name (case-insensitive). The empty
// string means auto.
func ParseMethod(s string) (Method, error) {
	switch m := Method(strings.ToLower(strings.TrimSpace(s))); m {
	case "":
		return MethodAuto, nil
	case MethodAuto, MethodTap, MethodDevice:
		return m, nil
	}
	return "", fmt.Errorf("%w %q (want auto, tap or device)", ErrUnknownMethod, s)
}

// Select resolves the requested method to a concrete backend. Auto picks
// the process tap when this Mac supports it and the capture device
// otherwise; an explicit tap request on a Mac that cannot run it fails
// with the reason instead of silently falling back.
func Select(requested Method, tap TapStatus) (Method, error) {
	switch requested {
	case MethodAuto, "":
		if tap.Supported {
			return MethodTap, nil
		}
		return MethodDevice, nil
	case MethodTap:
		if !tap.Supported {
			reason := tap.Reason
			if reason == "" {
				reason = "unsupported"
			}
			return "", fmt.Errorf("%w: %s", ErrTapUnsupported, reason)
		}
		return MethodTap, nil
	case MethodDevice:
		return MethodDevice, nil
	}
	return "", fmt.Errorf("%w %q (want auto, tap or device)", ErrUnknownMethod, string(requested))
}
