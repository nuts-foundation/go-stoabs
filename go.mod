module github.com/nuts-foundation/go-stoabs

go 1.18

replace github.com/go-redsync/redsync/v4 => github.com/reinkrul/redsync/v4 v4.5.2-0.20220801105510-6be544bc80cd

require (
	github.com/alicebob/miniredis/v2 v2.21.0
	github.com/go-redis/redis/v9 v9.0.0-beta.2
	github.com/go-redsync/redsync/v4 v4.5.1
	github.com/golang/mock v1.6.0
	github.com/sirupsen/logrus v1.8.1
	github.com/stretchr/testify v1.7.1
	go.etcd.io/bbolt v1.3.6
)

require (
	github.com/alicebob/gopher-json v0.0.0-20200520072559-a9ecdc9d1d3a // indirect
	github.com/cespare/xxhash/v2 v2.1.2 // indirect
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/dgryski/go-rendezvous v0.0.0-20200823014737-9f7001d12a5f // indirect
	github.com/hashicorp/errwrap v1.0.0 // indirect
	github.com/hashicorp/go-multierror v1.1.0 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	github.com/yuin/gopher-lua v0.0.0-20210529063254-f4c35e4016d9 // indirect
	golang.org/x/sys v0.0.0-20220520151302-bc2c85ada10a // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)
