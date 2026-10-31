module github.com/tracepanel/tracepanel-go/kafkatrace

go 1.26

require (
	github.com/segmentio/kafka-go v0.4.51
	github.com/tracepanel/tracepanel-go v0.10.0
)

require (
	github.com/klauspost/compress v1.15.9 // indirect
	github.com/pierrec/lz4/v4 v4.1.15 // indirect
)

replace github.com/tracepanel/tracepanel-go => ../
