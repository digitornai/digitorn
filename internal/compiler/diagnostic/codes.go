package diagnostic

// Code is a stable identifier of a diagnostic kind. Format: DGT-Exxxx (errors)
// or DGT-Wxxxx (warnings). Codes are never repurposed across releases.
type Code string

const (
	CodeYAMLSyntax        Code = "DGT-E0001"
	CodeDuplicateKey      Code = "DGT-E0002"
	CodeUnexpectedType    Code = "DGT-E0003"
	CodeEmptyDocument     Code = "DGT-E0004"
	CodeMultipleDocuments Code = "DGT-E0005"

	CodeMissingRequired Code = "DGT-E0100"
	CodeUnknownField    Code = "DGT-E0101"
	CodeWrongType       Code = "DGT-E0102"
	CodeOutOfRange      Code = "DGT-E0103"
	CodeBadEnum         Code = "DGT-E0104"
	CodeBadRegex        Code = "DGT-E0105"
	CodeBadLength       Code = "DGT-E0106"
	CodeBadSemver       Code = "DGT-E0107"
	CodeBadCron         Code = "DGT-E0108"
	CodeBadHexColor     Code = "DGT-E0109"
	CodeBadGlob         Code = "DGT-E0110"

	CodeUnknownNamespace     Code = "DGT-E0200"
	CodeMissingEnvVar        Code = "DGT-E0201"
	CodeMissingSecret        Code = "DGT-E0202"
	CodeMissingPromptFile    Code = "DGT-E0203"
	CodeMissingSkillFile     Code = "DGT-E0204"
	CodeMissingBehaviorFile  Code = "DGT-E0205"
	CodeMissingAssetFile     Code = "DGT-E0206"
	CodeMissingIncludeFile   Code = "DGT-E0207"
	CodePlaceholderCycle     Code = "DGT-E0208"
	CodeBadPlaceholderSyntax Code = "DGT-E0209"

	CodeUnknownAgent         Code = "DGT-E0300"
	CodeUnknownModule        Code = "DGT-E0301"
	CodeUnknownTool          Code = "DGT-E0302"
	CodeUnknownMiddleware    Code = "DGT-E0303"
	CodeUnknownHookEvent     Code = "DGT-E0304"
	CodeUnknownHookCondition Code = "DGT-E0305"
	CodeUnknownHookAction    Code = "DGT-E0306"
	CodeUnknownChannelType   Code = "DGT-E0307"
	CodeUnknownTriggerType   Code = "DGT-E0308"
	CodeUnknownProvider      Code = "DGT-E0309"
	CodeUnknownCredential    Code = "DGT-E0310"
	CodeUnknownConnectionID  Code = "DGT-E0311"
	CodeUnknownMCPServer     Code = "DGT-E0312"

	CodeDuplicateID      Code = "DGT-E0400"
	CodeCycleDelegate    Code = "DGT-E0401"
	CodeCycleFlow        Code = "DGT-E0402"
	CodeModeFieldMisuse  Code = "DGT-E0403"
	CodeUnreachableAgent Code = "DGT-E0404"
	CodeUnreachableNode  Code = "DGT-E0405"
	CodeNoEntryAgent     Code = "DGT-E0406"
	CodeMissingTrigger   Code = "DGT-E0407"
	CodeBadInputOutput   Code = "DGT-E0408"

	CodeImpossibleGrant      Code = "DGT-E0500"
	CodeOrphanCapability     Code = "DGT-E0501"
	CodeIncompatiblePlatform Code = "DGT-E0502"
	CodeMissingDependency    Code = "DGT-E0503"

	CodeBrainNoAuth         Code = "DGT-E0600"
	CodeSandboxBadResource  Code = "DGT-E0601"
	CodeCredentialFieldType Code = "DGT-E0602"
	CodeSecretLeak          Code = "DGT-E0603"

	CodeContextOverflow       Code = "DGT-E0700"
	CodeFallbackSameAsPrimary Code = "DGT-E0701"
	CodeVisionNotSupported    Code = "DGT-E0702"

	CodeBundleEntryMissing  Code = "DGT-E0800"
	CodeBundleBadStructure  Code = "DGT-E0801"
	CodeLocaleVariantUnused Code = "DGT-W0802"

	CodeCodegenFailed     Code = "DGT-E0900"
	CodeCodegenHashFailed Code = "DGT-E0901"

	CodeUnusedModule        Code = "DGT-W0001"
	CodeUnusedAgent         Code = "DGT-W0002"
	CodeUnusedMiddleware    Code = "DGT-W0003"
	CodeDeprecatedField     Code = "DGT-W0004"
	CodeExperimentalFeature Code = "DGT-W0005"
	CodeHookEventNotRouted  Code = "DGT-W0006"
	CodeUnknownEnumHint     Code = "DGT-W0007"
)
