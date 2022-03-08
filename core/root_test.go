package core

import (
	"net"
	"testing"
	"time"

	"github.com/Shopify/sarama"
	"github.com/golang/protobuf/proto"

	"akvorado/daemon"
	"akvorado/flow"
	"akvorado/geoip"
	"akvorado/helpers"
	"akvorado/kafka"
	"akvorado/reporter"
	"akvorado/snmp"
)

func TestCore(t *testing.T) {
	r := reporter.NewMock(t)

	// Prepare all components.
	daemonComponent := daemon.NewMock(t)
	snmpComponent := snmp.NewMock(t, r, snmp.DefaultConfiguration, snmp.Dependencies{Daemon: daemonComponent})
	flowComponent := flow.NewMock(t, r, flow.DefaultConfiguration)
	geoipComponent := geoip.NewMock(t, r)
	kafkaComponent, kafkaProducer := kafka.NewMock(t, r, kafka.DefaultConfiguration)

	// Instantiate and start core
	c, err := New(r, DefaultConfiguration, Dependencies{
		Daemon: daemonComponent,
		Flow:   flowComponent,
		Snmp:   snmpComponent,
		GeoIP:  geoipComponent,
		Kafka:  kafkaComponent,
	})
	if err != nil {
		t.Fatalf("New() error:\n%+v", err)
	}
	if err := c.Start(); err != nil {
		t.Fatalf("Start() error:\n%+v", err)
	}
	defer func() {
		if err := c.Stop(); err != nil {
			t.Fatalf("Stop() error:\n%+v", err)
		}
	}()

	// Inject several messages with a cache miss from the SNMP
	// component for each of them. No message sent to Kafka.
	flowMessage := func(router string, in, out uint32) *flow.FlowMessage {
		return &flow.FlowMessage{
			TimeReceived:   200,
			SequenceNum:    1000,
			SamplingRate:   1000,
			FlowDirection:  1,
			SamplerAddress: net.ParseIP(router),
			TimeFlowStart:  100,
			TimeFlowEnd:    200,
			Bytes:          6765,
			Packets:        4,
			InIf:           in,
			OutIf:          out,
			SrcAddr:        net.ParseIP("67.43.156.77"),
			DstAddr:        net.ParseIP("2.125.160.216"),
			Etype:          0x800,
			Proto:          6,
			SrcPort:        8534,
			DstPort:        80,
		}
	}

	flowComponent.Inject(t, flowMessage("192.0.2.142", 434, 677))
	flowComponent.Inject(t, flowMessage("192.0.2.143", 434, 677))
	flowComponent.Inject(t, flowMessage("192.0.2.143", 437, 677))
	flowComponent.Inject(t, flowMessage("192.0.2.143", 434, 679))

	time.Sleep(20 * time.Millisecond)
	gotMetrics := r.GetMetrics("akvorado_core_")
	expectedMetrics := map[string]string{
		`flows_errors{error="SNMP cache miss",router="192.0.2.142"}`: "1",
		`flows_errors{error="SNMP cache miss",router="192.0.2.143"}`: "3",
		`flows_received{router="192.0.2.142"}`:                       "1",
		`flows_received{router="192.0.2.143"}`:                       "3",
	}
	if diff := helpers.Diff(gotMetrics, expectedMetrics); diff != "" {
		t.Fatalf("Metrics (-got, +want):\n%s", diff)
	}

	// Inject again the messages, this time, we will get a cache hit!
	kafkaProducer.ExpectInputAndSucceed()
	flowComponent.Inject(t, flowMessage("192.0.2.142", 434, 677))
	kafkaProducer.ExpectInputAndSucceed()
	flowComponent.Inject(t, flowMessage("192.0.2.143", 437, 679))

	time.Sleep(20 * time.Millisecond)
	gotMetrics = r.GetMetrics("akvorado_core_")
	expectedMetrics = map[string]string{
		`flows_errors{error="SNMP cache miss",router="192.0.2.142"}`: "1",
		`flows_errors{error="SNMP cache miss",router="192.0.2.143"}`: "3",
		`flows_received{router="192.0.2.142"}`:                       "2",
		`flows_received{router="192.0.2.143"}`:                       "4",
		`flows_forwarded{router="192.0.2.142"}`:                      "1",
		`flows_forwarded{router="192.0.2.143"}`:                      "1",
	}
	if diff := helpers.Diff(gotMetrics, expectedMetrics); diff != "" {
		t.Fatalf("Metrics (-got, +want):\n%s", diff)
	}

	// Now, check we get the message we expect
	input := flowMessage("192.0.2.142", 434, 677)
	kafkaProducer.ExpectInputWithMessageCheckerFunctionAndSucceed(func(msg *sarama.ProducerMessage) error {
		if msg.Topic != "flows" {
			t.Errorf("Kafka message topic (-got, +want):\n-%s\n+%s", msg.Topic, "flows")
		}
		if msg.Key != sarama.StringEncoder("192.0.2.142") {
			t.Errorf("Kafka message key (-got, +want):\n-%s\n+%s", msg.Key, "192.0.2.142")
		}

		got := flow.FlowMessage{}
		b, err := msg.Value.Encode()
		if err != nil {
			t.Fatalf("Kafka message encoding error:\n%+v", err)
		}
		buf := proto.NewBuffer(b)
		err = buf.DecodeMessage(&got)
		if err != nil {
			t.Errorf("Kakfa message decode error:\n%+v", err)
		}
		expected := flowMessage("192.0.2.142", 434, 677)
		expected.SrcAS = 35908
		expected.SrcCountry = "BT"
		expected.DstAS = 0 // not in database
		expected.DstCountry = "GB"
		expected.InIfName = "Gi0/0/434"
		expected.OutIfName = "Gi0/0/677"
		expected.InIfDescription = "Interface 434"
		expected.OutIfDescription = "Interface 677"
		if diff := helpers.Diff(&got, expected); diff != "" {
			t.Errorf("Kafka message (-got, +want):\n%s", diff)
		}

		return nil
	})
	flowComponent.Inject(t, input)
	time.Sleep(20 * time.Millisecond)
}