module github.com/digitornai/digitorn

go 1.26.2

require (
	// HTTP routing
	github.com/go-chi/chi/v5 v5.1.0
	github.com/go-chi/cors v1.2.1

	// UUIDs
	github.com/google/uuid v1.6.0
	github.com/knadh/koanf/parsers/yaml v0.1.0
	github.com/knadh/koanf/providers/env v1.0.0
	github.com/knadh/koanf/providers/file v1.1.2

	// Config (lighter & cleaner than viper)
	github.com/knadh/koanf/v2 v2.1.1

	// Logging
	github.com/lmittmann/tint v1.0.5

	// CLI & TUI
	github.com/spf13/cobra v1.8.1

	// Socket.IO (only viable v4+ option in Go)
	github.com/zishang520/socket.io/servers/socket/v3 v3.0.3
	go.uber.org/automaxprocs v1.6.0

	// YAML parsing
	gopkg.in/yaml.v3 v3.0.1
	gorm.io/driver/mysql v1.5.7
	gorm.io/driver/postgres v1.5.9
	gorm.io/driver/sqlserver v1.5.3

	// ORM (multi-DB including Oracle via official Oracle driver)
	gorm.io/gorm v1.25.12
)

require (
	github.com/Azure/azure-sdk-for-go/sdk/azcore v1.20.0 // indirect
	github.com/Azure/azure-sdk-for-go/sdk/azidentity v1.13.1 // indirect
	github.com/andybalholm/brotli v1.2.1 // indirect
	github.com/dunglas/httpsfv v1.1.0 // indirect
	github.com/fsnotify/fsnotify v1.7.0 // indirect
	github.com/go-sql-driver/mysql v1.7.0
	github.com/go-viper/mapstructure/v2 v2.0.0-alpha.1 // indirect
	github.com/golang-sql/civil v0.0.0-20220223132316-b832511892a9 // indirect
	github.com/golang-sql/sqlexp v0.1.0 // indirect
	github.com/gookit/color v1.6.1 // indirect
	github.com/gorilla/websocket v1.5.3
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	// Oracle support: github.com/oracle-samples/gorm-oracle (add when needed - requires CGO)

	// Database driver pools
	github.com/jackc/pgx/v5 v5.9.1
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/jinzhu/inflection v1.0.0 // indirect
	github.com/jinzhu/now v1.1.5 // indirect
	github.com/klauspost/compress v1.18.6 // indirect
	github.com/knadh/koanf/maps v0.1.1 // indirect
	github.com/microsoft/go-mssqldb v1.6.0 // indirect
	github.com/mitchellh/copystructure v1.2.0 // indirect
	github.com/mitchellh/reflectwalk v1.0.2 // indirect
	github.com/quic-go/qpack v0.6.0 // indirect
	github.com/quic-go/quic-go v0.59.1 // indirect
	github.com/quic-go/webtransport-go v0.10.0 // indirect
	github.com/spf13/pflag v1.0.7 // indirect
	github.com/vmihailenco/msgpack/v5 v5.4.1 // indirect
	github.com/vmihailenco/tagparser/v2 v2.0.0 // indirect
	github.com/xo/terminfo v0.0.0-20220910002029-abceb7e1c41e // indirect
	github.com/zishang520/socket.io/parsers/engine/v3 v3.0.3 // indirect
	github.com/zishang520/socket.io/parsers/socket/v3 v3.0.3 // indirect
	github.com/zishang520/socket.io/servers/engine/v3 v3.0.3 // indirect
	github.com/zishang520/socket.io/v3 v3.0.3
	golang.org/x/crypto v0.52.0
	golang.org/x/net v0.55.0

	// Concurrency & resilience
	golang.org/x/sync v0.20.0
	golang.org/x/sys v0.45.0
	golang.org/x/text v0.37.0
)

require (
	github.com/JohannesKaufmann/html-to-markdown/v2 v2.5.1
	github.com/bytedance/sonic v1.15.0
	github.com/glebarez/sqlite v1.11.0
	github.com/go-git/go-billy/v5 v5.9.0
	github.com/go-git/go-git/v5 v5.19.1
	github.com/gocolly/colly/v2 v2.3.0
	github.com/golang-jwt/jwt/v5 v5.3.1
	github.com/jackc/pglogrepl v0.0.0-20260401131349-e37c41485510
	github.com/kardianos/service v1.2.4
	github.com/ledongthuc/pdf v0.0.0-20250511090121-5959a4027728
	github.com/maximhq/bifrost/core v1.5.13
	github.com/modelcontextprotocol/go-sdk v1.6.1
	github.com/qdrant/go-client v1.18.2
	github.com/redis/go-redis/v9 v9.20.1
	github.com/robfig/cron/v3 v3.0.1
	github.com/segmentio/kafka-go v0.4.51
	github.com/sergi/go-diff v1.4.0
	github.com/smacker/go-tree-sitter v0.0.0-20240827094217-dd81d9e9be82
	github.com/tiktoken-go/tokenizer v0.8.0
	github.com/tsawler/tabula v1.6.6
	github.com/yalue/onnxruntime_go v1.30.1
	go.mongodb.org/mongo-driver/v2 v2.6.0
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260427160629-7cedc36a6bc4
	google.golang.org/grpc v1.81.1
	mvdan.cc/sh/v3 v3.13.1
)

require (
	cloud.google.com/go v0.123.0 // indirect
	cloud.google.com/go/compute/metadata v0.9.0 // indirect
	dario.cat/mergo v1.0.2 // indirect
	github.com/Azure/azure-sdk-for-go/sdk/internal v1.11.2 // indirect
	github.com/AzureAD/microsoft-authentication-library-for-go v1.6.0 // indirect
	github.com/JohannesKaufmann/dom v0.2.0 // indirect
	github.com/Microsoft/go-winio v0.6.2 // indirect
	github.com/ProtonMail/go-crypto v1.1.6 // indirect
	github.com/PuerkitoBio/goquery v1.11.0 // indirect
	github.com/andybalholm/cascadia v1.3.3 // indirect
	github.com/antchfx/htmlquery v1.3.5 // indirect
	github.com/antchfx/xmlquery v1.5.0 // indirect
	github.com/antchfx/xpath v1.3.5 // indirect
	github.com/aws/aws-sdk-go-v2 v1.41.7 // indirect
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.7.10 // indirect
	github.com/aws/aws-sdk-go-v2/config v1.32.11 // indirect
	github.com/aws/aws-sdk-go-v2/credentials v1.19.14 // indirect
	github.com/aws/aws-sdk-go-v2/feature/ec2/imds v1.18.21 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.4.23 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.7.23 // indirect
	github.com/aws/aws-sdk-go-v2/internal/ini v1.8.5 // indirect
	github.com/aws/aws-sdk-go-v2/internal/v4a v1.4.22 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.7 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/checksum v1.9.13 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.13.21 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/s3shared v1.19.21 // indirect
	github.com/aws/aws-sdk-go-v2/service/s3 v1.97.3 // indirect
	github.com/aws/aws-sdk-go-v2/service/signin v1.0.9 // indirect
	github.com/aws/aws-sdk-go-v2/service/sso v1.30.15 // indirect
	github.com/aws/aws-sdk-go-v2/service/ssooidc v1.35.19 // indirect
	github.com/aws/aws-sdk-go-v2/service/sts v1.41.10 // indirect
	github.com/aws/smithy-go v1.25.1 // indirect
	github.com/aymanbagabas/go-pty v0.2.3 // indirect
	github.com/bahlo/generic-list-go v0.2.0 // indirect
	github.com/bits-and-blooms/bitset v1.24.4 // indirect
	github.com/buger/jsonparser v1.1.2 // indirect
	github.com/bytedance/gopkg v0.1.3 // indirect
	github.com/bytedance/sonic/loader v0.5.0 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/cloudflare/circl v1.6.3 // indirect
	github.com/cloudwego/base64x v0.1.6 // indirect
	github.com/creack/pty v1.1.24 // indirect
	github.com/cyphar/filepath-securejoin v0.6.1 // indirect
	github.com/dlclark/regexp2/v2 v2.1.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/emirpasic/gods v1.18.1 // indirect
	github.com/glebarez/go-sqlite v1.21.2 // indirect
	github.com/go-git/gcfg v1.5.1-0.20230307220236-3a3c6141e376 // indirect
	github.com/gobwas/glob v0.2.3 // indirect
	github.com/golang/groupcache v0.0.0-20241129210726-2c02b8208cf8 // indirect
	github.com/golang/protobuf v1.5.4 // indirect
	github.com/google/jsonschema-go v0.4.3 // indirect
	github.com/invopop/jsonschema v0.13.0 // indirect
	github.com/jackc/pgio v1.0.0 // indirect
	github.com/jbenet/go-context v0.0.0-20150711004518-d14ea06fba99 // indirect
	github.com/kennygrant/sanitize v1.2.4 // indirect
	github.com/kevinburke/ssh_config v1.2.0 // indirect
	github.com/klauspost/cpuid/v2 v2.3.0 // indirect
	github.com/kylelemons/godebug v1.1.0 // indirect
	github.com/mailru/easyjson v0.9.1 // indirect
	github.com/mark3labs/mcp-go v0.43.2 // indirect
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.22 // indirect
	github.com/nlnwa/whatwg-url v0.6.2 // indirect
	github.com/otiai10/gosseract/v2 v2.4.1 // indirect
	github.com/pierrec/lz4/v4 v4.1.22 // indirect
	github.com/pjbgf/sha1cd v0.6.0 // indirect
	github.com/pkg/browser v0.0.0-20240102092130-5ac0b6a4141c // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/rs/zerolog v1.34.0 // indirect
	github.com/saintfish/chardet v0.0.0-20230101081208-5e3ef4b5456d // indirect
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/skeema/knownhosts v1.3.1 // indirect
	github.com/spf13/cast v1.10.0 // indirect
	github.com/temoto/robotstxt v1.1.2 // indirect
	github.com/tidwall/gjson v1.18.0 // indirect
	github.com/tidwall/match v1.1.1 // indirect
	github.com/tidwall/pretty v1.2.0 // indirect
	github.com/tidwall/sjson v1.2.5 // indirect
	github.com/twitchyliquid64/golang-asm v0.15.1 // indirect
	github.com/u-root/u-root v0.16.0 // indirect
	github.com/valyala/bytebufferpool v1.0.0 // indirect
	github.com/valyala/fasthttp v1.68.0 // indirect
	github.com/wk8/go-ordered-map/v2 v2.1.8 // indirect
	github.com/xanzy/ssh-agent v0.3.3 // indirect
	github.com/xdg-go/pbkdf2 v1.0.0 // indirect
	github.com/xdg-go/scram v1.2.0 // indirect
	github.com/xdg-go/stringprep v1.0.4 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	github.com/youmark/pkcs8 v0.0.0-20240726163527-a2c0da244d78 // indirect
	go.starlark.net v0.0.0-20260102030733-3fee463870c9 // indirect
	go.uber.org/atomic v1.11.0 // indirect
	golang.org/x/arch v0.23.0 // indirect
	golang.org/x/image v0.18.0 // indirect
	golang.org/x/oauth2 v0.36.0 // indirect
	golang.org/x/term v0.43.0 // indirect
	google.golang.org/appengine v1.6.8 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
	gopkg.in/warnings.v0 v0.1.2 // indirect
	modernc.org/libc v1.22.5 // indirect
	modernc.org/mathutil v1.5.0 // indirect
	modernc.org/memory v1.5.0 // indirect
	modernc.org/sqlite v1.23.1 // indirect
)
