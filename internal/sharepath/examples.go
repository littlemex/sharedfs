package sharepath

import "strings"

// ValidExamples and InvalidExamples are shared by the tests of Parse and of the CRD's CEL rule,
// which is a separate implementation of the same rules: every valid path must be accepted by
// both, every invalid one rejected by both.
var (
	ValidExamples = []string{
		"/", "datasets", "checkpoints/{namespace}", "models/llama-3.1/8B_instruct",
		"a/b/c", "{namespace}", "teams/{namespace}/ckpt", "_x", "x.y",
		strings.Repeat("a", 255), strings.TrimSuffix(strings.Repeat("a/", 32), "/"),
	}
	InvalidExamples = []string{
		"", "//", "/datasets", "datasets/", "a//b", ".", "..", "a/../b", "a/./b", ".lustre",
		"a/.hidden", "-flag", "a/-b", "a b", "a\nb", "a/{ns}", "{namespace}x", "x{namespace}",
		"ü", strings.Repeat("a", 256), strings.TrimSuffix(strings.Repeat("a/", 33), "/"),
		strings.Repeat("abcdefgh/", 120) + "x",
	}
)
