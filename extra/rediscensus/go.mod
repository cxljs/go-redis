module github.com/redis/go-redis/extra/rediscensus/v9

go 1.19

replace github.com/redis/go-redis/v9 => ../..

replace github.com/redis/go-redis/extra/rediscmd/v9 => ../rediscmd

require (
	github.com/redis/go-redis/extra/rediscmd/v9 v9.11.0
	github.com/redis/go-redis/v9 v9.11.0
	go.opencensus.io v0.24.0
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dgryski/go-rendezvous v0.0.0-20200823014737-9f7001d12a5f // indirect
	github.com/golang/groupcache v0.0.0-20210331224755-41bb18bfe9da // indirect
)

retract (
	v9.7.2 // This version was accidentally released.
	v9.5.3 // This version was accidentally released.
)
