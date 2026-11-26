// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package docscli

import "github.com/cameronsjo/forgectl/internal/termsafe"

const (
	labelMaxRunes = 64
	titleMaxRunes = 256
	textMaxRunes  = 1280
)

func safeLabel(s string) string {
	return termsafe.SafeLineMax(s, labelMaxRunes)
}

func safeTitle(s string) string {
	return termsafe.SafeLineMax(s, titleMaxRunes)
}

func safeText(s string) string {
	return termsafe.SafeLineMax(s, textMaxRunes)
}

func safeColumnPath(s string) string {
	return termsafe.SafePathMax(s, termsafe.PathEchoMaxRunes)
}
