package kafkainbox

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/jsell-rh/stego/registry/runtime/outbox/kafkaoutbox"
)

// The settings of the Kafka inbox, which the worker mode reads from the process environment by these fixed names
// (ST-SUP-RP01-LC38). The connection settings are the settings of the Kafka destination (ST-SUP-RP01-LC32).
const (
	// TopicSetting is the topic that the inbox reads: the topic of the source service's change events.
	TopicSetting     = kafkaoutbox.ConsumerTopicSetting
	TopicFileSetting = kafkaoutbox.ConsumerTopicFileSetting
	// GroupSetting is the consumer group. Unset: <service>.<subscription>.
	GroupSetting     = kafkaoutbox.ConsumerGroupSetting
	GroupFileSetting = kafkaoutbox.ConsumerGroupFileSetting
	// InfiniteRetentionSetting set to 1 lets the inbox read a topic whose retention is infinite. The inbox keeps its
	// dedup rows for the done retention of the outbox only, so an offset reset to a record older than that can apply
	// it again; the setting says that the operator accepts this.
	InfiniteRetentionSetting = "STEGO_KAFKA_CONSUMER_ACCEPT_INFINITE_RETENTION"
)

// topicPattern is a Kafka topic name, and groupPattern a consumer group name of the inbox.
var (
	topicPattern = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,249}$`)
	groupPattern = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,249}$`)
)

// Settings are the settings of the Kafka inbox.
type Settings struct {
	// Connection are the connection settings, which the Kafka destination reads too. Their topic is not read.
	Connection kafkaoutbox.Settings
	// Topic is the topic that the inbox reads, and Group its consumer group.
	Topic, Group string
	// AcceptInfiniteRetention lets the inbox read a topic with an infinite retention.
	AcceptInfiniteRetention bool
}

// SettingError is the refusal of one setting of the inbox: it names the setting and holds no value of it.
type SettingError struct {
	Variable, Problem string
}

func (e *SettingError) Error() string {
	return fmt.Sprintf("kafkainbox: the setting %s %s", e.Variable, e.Problem)
}

// ReadSettings reads the settings of the inbox of the subscription subscription of the service service from the
// process environment: the connection settings as the Kafka destination reads them, the topic, which must be set,
// and the consumer group, which is <service>.<subscription> when it is not set (ST-SUP-RP01-LC38). It refuses a
// setting that it cannot use with an error that names the setting and holds no value of it.
func ReadSettings(service, subscription string) (Settings, error) {
	connection, err := kafkaoutbox.ReadSettings()
	if err != nil {
		return Settings{}, err
	}
	connection.Topic = ""
	s := Settings{Connection: connection}
	if s.Topic, s.Group, err = kafkaoutbox.ReadConsumer(); err != nil {
		return Settings{}, err
	}
	if s.Group == "" {
		s.Group = service + "." + subscription
	}
	switch os.Getenv(InfiniteRetentionSetting) {
	case "", "0":
	case "1":
		s.AcceptInfiniteRetention = true
	default:
		return Settings{}, &SettingError{Variable: InfiniteRetentionSetting, Problem: "must be 0 or 1"}
	}
	return s, s.Check()
}

// Check refuses settings that the inbox cannot use: connection settings that the Kafka destination refuses, no
// topic, a topic that Kafka does not take or keeps for itself, and a group that is not a name of letters, digits,
// '.', '_' and '-'.
func (s Settings) Check() error {
	if err := s.Connection.Check(); err != nil {
		return err
	}
	switch {
	case s.Topic == "":
		return &SettingError{Variable: TopicSetting, Problem: "or " + TopicFileSetting + " must be set for the worker mode: it is the topic of the subscription"}
	case !topicPattern.MatchString(s.Topic) || s.Topic == "." || s.Topic == ".." || strings.HasPrefix(s.Topic, "__"):
		return &SettingError{Variable: TopicSetting, Problem: "must be a Kafka topic name that Kafka does not keep for itself"}
	case !groupPattern.MatchString(s.Group):
		return &SettingError{Variable: GroupSetting, Problem: "must be a group name of 1 to 249 letters, digits, '.', '_' or '-'"}
	}
	return nil
}
