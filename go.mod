module go.keploy.io/server

go 1.16

//replace github.com/keploy/go-sdk => ../go-sdk

require (
	github.com/99designs/gqlgen v0.15.1 // v should be less or equal 0.15.1
	github.com/anmitsu/go-shlex v0.0.0-20200514113438-38f4b401e2be // indirect
	github.com/araddon/dateparse v0.0.0-20210429162001-6b43995a97de
	github.com/emirpasic/gods v1.18.1 // indirect
	github.com/gliderlabs/ssh v0.3.2 // indirect
	github.com/go-chi/chi v1.5.4
	github.com/go-chi/cors v1.2.0
	github.com/go-chi/render v1.0.1
	github.com/go-git/go-git-fixtures/v4 v4.3.1 // indirect
	github.com/go-git/go-git/v5 v5.2.0
	github.com/go-test/deep v1.0.8
	github.com/google/uuid v1.3.0
	github.com/hashicorp/go-version v1.6.0
	github.com/imdario/mergo v0.3.13 // indirect
	github.com/k0kubun/pp/v3 v3.1.0
	github.com/kelseyhightower/envconfig v1.4.0
	github.com/keploy/go-sdk v0.8.6
	github.com/kevinburke/ssh_config v1.2.0 // indirect
	github.com/soheilhy/cmux v0.1.5
	github.com/vektah/gqlparser/v2 v2.2.0 // v should b4 less or equal 2.2.0
	github.com/wI2L/jsondiff v0.3.0
	go.mongodb.org/mongo-driver v1.8.3
	go.uber.org/zap v1.22.0
	golang.org/x/mod v0.6.0-dev // indirect
	golang.org/x/sync v0.1.0
	google.golang.org/grpc v1.48.0
	google.golang.org/protobuf v1.28.1
	gopkg.in/yaml.v3 v3.0.1
)
