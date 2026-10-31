module github.com/tracepanel/tracepanel-go/zaptrace

go 1.26

require (
	github.com/tracepanel/tracepanel-go v0.10.0
	go.uber.org/zap v1.28.0
)

require go.uber.org/multierr v1.10.0 // indirect

replace github.com/tracepanel/tracepanel-go => ../
