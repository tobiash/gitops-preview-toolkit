module github.com/tobiash/gitops-preview-toolkit

go 1.26.3

require (
	github.com/containerd/errdefs v1.0.0
	github.com/crossplane/cli/v2 v2.5.0
	github.com/crossplane/crossplane-runtime/v2 v2.3.3
	github.com/crossplane/crossplane/apis/v2 v2.3.4
	github.com/crossplane/function-sdk-go v0.7.1
	github.com/docker/docker v28.5.2+incompatible
	github.com/fluxcd/helm-controller/api v1.5.3
	github.com/fluxcd/kustomize-controller/api v1.6.1
	github.com/fluxcd/pkg/apis/kustomize v1.15.1
	github.com/fluxcd/pkg/chartutil v1.22.1
	github.com/fluxcd/pkg/git v0.43.1
	github.com/fluxcd/source-controller/api v1.8.1
	github.com/go-logr/logr v1.4.3
	github.com/go-logr/zerologr v1.2.3
	github.com/google/jsonschema-go v0.3.0
	github.com/google/uuid v1.6.0
	github.com/hexops/gotextdiff v1.0.3
	github.com/modelcontextprotocol/go-sdk v1.2.0
	github.com/open-policy-agent/opa v1.15.2
	github.com/rs/zerolog v1.33.0
	github.com/sethvargo/go-githubactions v1.3.2
	github.com/spf13/cobra v1.10.2
	github.com/spf13/pflag v1.0.10
	github.com/tmc/langchaingo v0.1.14
	github.com/tobiash/k8q v0.3.1-0.20260916190906-a8e82e78d367
	golang.org/x/sync v0.22.0
	google.golang.org/grpc v1.82.1
	google.golang.org/protobuf v1.36.12
	gopkg.in/yaml.v3 v3.0.1
	helm.sh/helm/v4 v4.1.3
	k8s.io/api v0.35.3
	k8s.io/apiextensions-apiserver v0.35.2
	k8s.io/apimachinery v0.35.3
	k8s.io/client-go v0.35.3
	k8s.io/kube-openapi v0.0.0-20260127142750-a19766b6e2d4
	sigs.k8s.io/controller-runtime v0.23.3
	sigs.k8s.io/kustomize/api v0.21.1
	sigs.k8s.io/kustomize/kyaml v0.21.1
	sigs.k8s.io/yaml v1.6.0
)

require (
	cel.dev/expr v0.25.1 // indirect
	dario.cat/mergo v1.0.2 // indirect
	filippo.io/edwards25519 v1.1.1 // indirect
	github.com/Azure/go-ansiterm v0.0.0-20250102033503-faa5f7b0171c // indirect
	github.com/BurntSushi/toml v1.6.0 // indirect
	github.com/MakeNowJust/heredoc v1.0.0 // indirect
	github.com/Masterminds/goutils v1.1.1 // indirect
	github.com/Masterminds/semver/v3 v3.4.0 // indirect
	github.com/Masterminds/sprig/v3 v3.3.0 // indirect
	github.com/Masterminds/squirrel v1.5.4 // indirect
	github.com/Microsoft/go-winio v0.6.2 // indirect
	github.com/ProtonMail/go-crypto v1.3.0 // indirect
	github.com/agnivade/levenshtein v1.2.1 // indirect
	github.com/antlr4-go/antlr/v4 v4.13.1 // indirect
	github.com/asaskevich/govalidator v0.0.0-20230301143203-a9d515a09cc2 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/blang/semver/v4 v4.0.0 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/chai2010/gettext-go v1.0.2 // indirect
	github.com/clipperhouse/stringish v0.1.1 // indirect
	github.com/clipperhouse/uax29/v2 v2.5.0 // indirect
	github.com/cloudflare/circl v1.6.3 // indirect
	github.com/containerd/errdefs/pkg v0.3.0 // indirect
	github.com/cyphar/filepath-securejoin v0.6.1 // indirect
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/decred/dcrd/dcrec/secp256k1/v4 v4.4.0 // indirect
	github.com/distribution/reference v0.6.0 // indirect
	github.com/dlclark/regexp2 v1.11.5 // indirect
	github.com/docker/cli v29.7.1+incompatible // indirect
	github.com/docker/docker-credential-helpers v0.9.6 // indirect
	github.com/docker/go-connections v0.8.1 // indirect
	github.com/docker/go-units v0.5.0 // indirect
	github.com/dylibso/observe-sdk/go v0.0.0-20240819160327-2d926c5d788a // indirect
	github.com/emicklei/go-restful/v3 v3.13.0 // indirect
	github.com/emirpasic/gods v1.18.1 // indirect
	github.com/evanphx/json-patch/v5 v5.9.11 // indirect
	github.com/exponent-io/jsonpath v0.0.0-20210407135951-1de76d718b3f // indirect
	github.com/extism/go-sdk v1.7.1 // indirect
	github.com/fatih/color v1.18.0 // indirect
	github.com/felixge/httpsnoop v1.0.4 // indirect
	github.com/fluxcd/cli-utils v0.37.2-flux.1 // indirect
	github.com/fluxcd/pkg/apis/acl v0.9.0 // indirect
	github.com/fluxcd/pkg/apis/meta v1.25.1 // indirect
	github.com/fluxcd/pkg/ssh v0.24.0 // indirect
	github.com/fluxcd/pkg/version v0.12.0 // indirect
	github.com/fxamacker/cbor/v2 v2.9.0 // indirect
	github.com/go-errors/errors v1.5.1 // indirect
	github.com/go-git/gcfg v1.5.1-0.20230307220236-3a3c6141e376 // indirect
	github.com/go-git/go-billy/v5 v5.9.1 // indirect
	github.com/go-git/go-git/v5 v5.19.2 // indirect
	github.com/go-gorp/gorp/v3 v3.1.0 // indirect
	github.com/go-json-experiment/json v0.0.0-20240815175050-ebd3a8989ca1 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/go-openapi/jsonpointer v0.23.1 // indirect
	github.com/go-openapi/jsonreference v0.21.6 // indirect
	github.com/go-openapi/swag v0.26.0 // indirect
	github.com/go-openapi/swag/cmdutils v0.26.0 // indirect
	github.com/go-openapi/swag/conv v0.26.0 // indirect
	github.com/go-openapi/swag/fileutils v0.26.0 // indirect
	github.com/go-openapi/swag/jsonname v0.26.0 // indirect
	github.com/go-openapi/swag/jsonutils v0.26.0 // indirect
	github.com/go-openapi/swag/loading v0.26.0 // indirect
	github.com/go-openapi/swag/mangling v0.26.0 // indirect
	github.com/go-openapi/swag/netutils v0.26.0 // indirect
	github.com/go-openapi/swag/stringutils v0.26.0 // indirect
	github.com/go-openapi/swag/typeutils v0.26.0 // indirect
	github.com/go-openapi/swag/yamlutils v0.26.0 // indirect
	github.com/gobuffalo/flect v1.0.3 // indirect
	github.com/gobwas/glob v0.2.3 // indirect
	github.com/goccy/go-json v0.10.5 // indirect
	github.com/gofrs/flock v0.13.0 // indirect
	github.com/golang/groupcache v0.0.0-20241129210726-2c02b8208cf8 // indirect
	github.com/google/btree v1.1.3 // indirect
	github.com/google/cel-go v0.30.0 // indirect
	github.com/google/gnostic-models v0.7.1 // indirect
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/google/go-containerregistry v0.21.9 // indirect
	github.com/gosuri/uitable v0.0.4 // indirect
	github.com/gregjones/httpcache v0.0.0-20190611155906-901d90724c79 // indirect
	github.com/huandu/xstrings v1.5.0 // indirect
	github.com/ianlancetaylor/demangle v0.0.0-20240805132620-81f5be970eca // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/jbenet/go-context v0.0.0-20150711004518-d14ea06fba99 // indirect
	github.com/jmoiron/sqlx v1.4.0 // indirect
	github.com/json-iterator/go v1.1.13-0.20220915233716-71ac16282d12 // indirect
	github.com/kevinburke/ssh_config v1.2.0 // indirect
	github.com/klauspost/cpuid/v2 v2.3.0 // indirect
	github.com/lann/builder v0.0.0-20180802200727-47ae307949d0 // indirect
	github.com/lann/ps v0.0.0-20150810152359-62de8c46ede0 // indirect
	github.com/lestrrat-go/blackmagic v1.0.4 // indirect
	github.com/lestrrat-go/dsig v1.0.0 // indirect
	github.com/lestrrat-go/dsig-secp256k1 v1.0.0 // indirect
	github.com/lestrrat-go/httpcc v1.0.1 // indirect
	github.com/lestrrat-go/httprc/v3 v3.0.2 // indirect
	github.com/lestrrat-go/jwx/v3 v3.0.13 // indirect
	github.com/lestrrat-go/option/v2 v2.0.0 // indirect
	github.com/lib/pq v1.11.2 // indirect
	github.com/liggitt/tabwriter v0.0.0-20181228230101-89fcab3d43de // indirect
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.22 // indirect
	github.com/mattn/go-runewidth v0.0.19 // indirect
	github.com/mitchellh/copystructure v1.2.0 // indirect
	github.com/mitchellh/go-wordwrap v1.0.1 // indirect
	github.com/mitchellh/reflectwalk v1.0.2 // indirect
	github.com/moby/docker-image-spec v1.3.1 // indirect
	github.com/moby/term v0.5.2 // indirect
	github.com/modern-go/concurrent v0.0.0-20180306012644-bacd9c7ef1dd // indirect
	github.com/modern-go/reflect2 v1.0.3-0.20250322232337-35a7c28c31ee // indirect
	github.com/monochromegane/go-gitignore v0.0.0-20200626010858-205db1a8cc00 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	github.com/opencontainers/image-spec v1.1.1 // indirect
	github.com/peterbourgon/diskv v2.0.1+incompatible // indirect
	github.com/pjbgf/sha1cd v0.6.0 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/pkoukk/tiktoken-go v0.1.6 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/prometheus/client_golang v1.23.2 // indirect
	github.com/prometheus/client_model v0.6.2 // indirect
	github.com/prometheus/common v0.67.5 // indirect
	github.com/prometheus/procfs v0.20.1 // indirect
	github.com/rcrowley/go-metrics v0.0.0-20250401214520-65e299d6c5c9 // indirect
	github.com/rubenv/sql-migrate v1.8.1 // indirect
	github.com/russross/blackfriday/v2 v2.1.0 // indirect
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.2 // indirect
	github.com/segmentio/asm v1.2.1 // indirect
	github.com/sergi/go-diff v1.4.0 // indirect
	github.com/shopspring/decimal v1.4.0 // indirect
	github.com/sirupsen/logrus v1.9.4 // indirect
	github.com/skeema/knownhosts v1.3.1 // indirect
	github.com/spf13/afero v1.15.0 // indirect
	github.com/spf13/cast v1.10.0 // indirect
	github.com/tchap/go-patricia/v2 v2.3.3 // indirect
	github.com/tetratelabs/wabin v0.0.0-20230304001439-f6f874872834 // indirect
	github.com/tetratelabs/wazero v1.11.0 // indirect
	github.com/valyala/fastjson v1.6.7 // indirect
	github.com/vektah/gqlparser/v2 v2.5.32 // indirect
	github.com/x448/float16 v0.8.4 // indirect
	github.com/xanzy/ssh-agent v0.3.3 // indirect
	github.com/xeipuuv/gojsonpointer v0.0.0-20190905194746-02993c407bfb // indirect
	github.com/xeipuuv/gojsonreference v0.0.0-20180127040603-bd5ef7bd5415 // indirect
	github.com/xlab/treeprint v1.2.0 // indirect
	github.com/yashtewari/glob-intersection v0.2.0 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.68.0 // indirect
	go.opentelemetry.io/otel v1.44.0 // indirect
	go.opentelemetry.io/otel/metric v1.44.0 // indirect
	go.opentelemetry.io/otel/trace v1.44.0 // indirect
	go.opentelemetry.io/proto/otlp v1.10.0 // indirect
	go.yaml.in/yaml/v2 v2.4.4 // indirect
	go.yaml.in/yaml/v3 v3.0.4 // indirect
	golang.org/x/crypto v0.55.0 // indirect
	golang.org/x/exp v0.0.0-20260410095643-746e56fc9e2f // indirect
	golang.org/x/mod v0.40.0 // indirect
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/oauth2 v0.36.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/term v0.45.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	golang.org/x/time v0.15.0 // indirect
	golang.org/x/tools v0.49.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260526163538-3dc84a4a5aaa // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260523011958-0a33c5d7ca68 // indirect
	gopkg.in/evanphx/json-patch.v4 v4.13.0 // indirect
	gopkg.in/inf.v0 v0.9.1 // indirect
	gopkg.in/warnings.v0 v0.1.2 // indirect
	gopkg.in/yaml.v2 v2.4.0 // indirect
	gotest.tools/v3 v3.5.2 // indirect
	k8s.io/apiserver v0.35.2 // indirect
	k8s.io/cli-runtime v0.35.1 // indirect
	k8s.io/code-generator v0.35.2 // indirect
	k8s.io/component-base v0.35.2 // indirect
	k8s.io/gengo/v2 v2.0.0-20251215205346-5ee0d033ba5b // indirect
	k8s.io/klog/v2 v2.140.0 // indirect
	k8s.io/kubectl v0.35.1 // indirect
	k8s.io/utils v0.0.0-20260319190234-28399d86e0b5 // indirect
	oras.land/oras-go/v2 v2.6.2 // indirect
	sigs.k8s.io/controller-tools v0.20.0 // indirect
	sigs.k8s.io/json v0.0.0-20250730193827-2d320260d730 // indirect
	sigs.k8s.io/randfill v1.0.0 // indirect
	sigs.k8s.io/structured-merge-diff/v6 v6.3.2-0.20260122202528-d9cc6641c482 // indirect
)
