module github.com/meshplus/pier-client-ethereum

go 1.13

require (
	github.com/Rican7/retry v0.1.0
	github.com/StackExchange/wmi v1.2.1 // indirect
	github.com/ethereum/go-ethereum v1.10.4
	github.com/fatih/color v1.9.0
	github.com/gobuffalo/packd v1.0.0
	github.com/gobuffalo/packr v1.30.1
	github.com/hashicorp/go-hclog v0.0.0-20180709165350-ff2cf002a8dd
	github.com/hashicorp/go-plugin v1.3.0
	github.com/meshplus/bitxhub-model v1.2.1-0.20210929032626-e9a7f829028f
	github.com/meshplus/pier v1.12.1-0.20211009015116-a0ca969c2dfc
	github.com/spf13/viper v1.7.1
	github.com/urfave/cli v1.22.1
)

replace github.com/ethereum/go-ethereum => github.com/ethereum/go-ethereum v1.10.2
