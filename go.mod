module go.keploy.io/server

go 1.16

//replace github.com/keploy/go-sdk => ../go-sdk

require (
	github.com/99designs/gqlgen v0.15.1 // v should be less or equal 0.15.1
	github.com/araddon/dateparse v0.0.0-20210429162001-6b43995a97de
	github.com/go-chi/chi v1.5.4
	github.com/go-chi/cors v1.2.0
	github.com/go-chi/render v1.0.1
	github.com/go-git/go-git/v5 v5.6.1
	github.com/go-test/deep v1.1.0
	github.com/google/uuid v1.3.0
	github.com/hashicorp/go-version v1.6.0
	github.com/imdario/mergo v0.3.14 // indirect
	github.com/k0kubun/pp/v3 v3.1.0
	github.com/kelseyhightower/envconfig v1.4.0
	github.com/keploy/go-sdk v0.8.6
	github.com/soheilhy/cmux v0.1.5
	github.com/vektah/gqlparser/v2 v2.2.0 // v should b4 less or equal 2.2.0
	github.com/wI2L/jsondiff v0.3.0
	go.mongodb.org/mongo-driver v1.8.3
	go.uber.org/zap v1.22.0
	google.golang.org/protobuf v1.28.1
)

require (
	github.com/fatih/color v1.15.0
	github.com/olekukonko/tablewriter v0.0.5
	github.com/yudai/gojsondiff v1.0.0
	github.com/yudai/golcs v0.0.0-20170316035057-ecda9a501e82 // indirect
)

require (
	golang.org/x/sync v0.1.0
	google.golang.org/grpc v1.48.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/Microsoft/go-winio v0.6.0 // indirect
	github.com/ProtonMail/go-crypto v0.0.0-20230321155629-9a39f2531310 // indirect
	github.com/cloudflare/circl v1.3.2 // indirect
	github.com/mattn/go-runewidth v0.0.14 // indirect
	github.com/sergi/go-diff v1.3.1 // indirect
	github.com/stretchr/testify v1.8.0 // indirect
	github.com/yudai/pp v2.0.1+incompatible // indirect
	golang.org/x/tools v0.7.0 // indirect
)
