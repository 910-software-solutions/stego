package kafkainbox

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/jsell-rh/stego/registry/runtime/outbox/kafkaoutbox"
)

// The settings of the Kafka inbox of one subscription, which the worker mode reads from the process environment by
// the name of the subscription in place of {subscription}: the name in capital letters with '_' in place of '-'
// (ST-SUP-RP01-LC38, decision on ST-SUP-Q204). The connection settings are the settings of the Kafka destination
// (ST-SUP-RP01-LC32).
const (
	// SubscriptionTopicSetting is the topic that the inbox reads: the topic of the source service's change events.
	// These names are those of kafkaoutbox.ConsumerSubscriptionTopicSetting and the others, which read them, as text,
	// so that the release scan reads them here too (TestSettings holds them equal).
	SubscriptionTopicSetting     = "STEGO_KAFKA_CONSUMER_{subscription}_TOPIC"
	SubscriptionTopicFileSetting = "STEGO_KAFKA_CONSUMER_{subscription}_TOPIC_FILE"
	// SubscriptionGroupSetting is the consumer group. Unset: <service>.<subscription>.
	SubscriptionGroupSetting     = "STEGO_KAFKA_CONSUMER_{subscription}_GROUP"
	SubscriptionGroupFileSetting = "STEGO_KAFKA_CONSUMER_{subscription}_GROUP_FILE"
	// SubscriptionInfiniteRetentionSetting set to 1 lets the inbox read a topic whose retention is infinite. The
	// inbox keeps its dedup rows for the done retention of the outbox only, so an offset reset to a record older than
	// that can apply it again; the setting says that the operator accepts this.
	SubscriptionInfiniteRetentionSetting = "STEGO_KAFKA_CONSUMER_{subscription}_ACCEPT_INFINITE_RETENTION"
)

// The fixed names of the settings of kafka-consumer before 0.5.0, which named the one subscription of a service. They
// stay for the API of the runtime (ST-STD-RUNTIME-R03); the inbox reads them only to refuse a value.
//
// Deprecated: use SubscriptionTopicSetting and the others with Setting.
const (
	TopicSetting             = kafkaoutbox.ConsumerTopicSetting
	TopicFileSetting         = kafkaoutbox.ConsumerTopicFileSetting
	GroupSetting             = kafkaoutbox.ConsumerGroupSetting
	GroupFileSetting         = kafkaoutbox.ConsumerGroupFileSetting
	InfiniteRetentionSetting = "STEGO_KAFKA_CONSUMER_ACCEPT_INFINITE_RETENTION"
)

// formerSettings are the fixed names of the settings of kafka-consumer before 0.5.0, which named the one subscription
// of a service, each with the setting of a subscription that takes its place. The inbox reads them only to refuse a
// value of each, so that a value is never ignored in silence (ST-SUP-RP01-LC38, decision on ST-SUP-Q204).
var formerSettings = []formerSetting{
	{name: "STEGO_KAFKA_CONSUMER_TOPIC", setting: SubscriptionTopicSetting},
	{name: "STEGO_KAFKA_CONSUMER_TOPIC_FILE", setting: SubscriptionTopicFileSetting},
	{name: "STEGO_KAFKA_CONSUMER_GROUP", setting: SubscriptionGroupSetting},
	{name: "STEGO_KAFKA_CONSUMER_GROUP_FILE", setting: SubscriptionGroupFileSetting},
	{name: "STEGO_KAFKA_CONSUMER_ACCEPT_INFINITE_RETENTION", setting: SubscriptionInfiniteRetentionSetting},
}

// formerSetting is a setting of kafka-consumer before 0.5.0 and the setting of a subscription in its place.
type formerSetting struct{ name, setting string }

// Setting gives the name of the setting setting, such as SubscriptionTopicSetting, of the subscription subscription, such as
// STEGO_KAFKA_CONSUMER_ORDER_CHANGES_TOPIC for order-changes.
func Setting(setting, subscription string) string {
	return kafkaoutbox.SubscriptionSetting(setting, subscription)
}

// topicPattern is a Kafka topic name, and groupPattern a consumer group name of the inbox.
var (
	topicPattern = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,249}$`)
	groupPattern = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,249}$`)
)

// Settings are the settings of the Kafka inbox of one subscription.
type Settings struct {
	// Subscription is the name of the subscription, which names the settings of its topic and its group.
	Subscription string
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

// CheckFormer refuses a value of a setting of kafka-consumer before 0.5.0 with an error that names the setting of the
// first subscription of subscriptions that takes its place; with no such value it gives nil (ST-SUP-RP01-LC38).
func CheckFormer(subscriptions []string) error {
	if len(subscriptions) == 0 {
		return nil
	}
	for _, former := range formerSettings {
		if os.Getenv(former.name) == "" {
			continue
		}
		instead := Setting(former.setting, subscriptions[0])
		if len(subscriptions) > 1 {
			instead = fmt.Sprintf("%s for the subscription %s, and the setting of each other subscription by its name", instead, subscriptions[0])
		}
		return &SettingError{Variable: former.name, Problem: "is the name of kafka-consumer before 0.5.0, which takes the settings of each subscription by its name: set " + instead + " in its place"}
	}
	return nil
}

// CheckSettings refuses, at the start of the worker mode of the service service, a value of a setting of
// kafka-consumer before 0.5.0 (CheckFormer), a subscription of subscriptions whose topic has no setting, a group
// or topic setting that cannot be read, and two subscriptions with one consumer group, each with a SettingError that
// names the setting and holds no value (ST-SUP-RP14-LC08, decision on ST-SUP-Q204). The group of a subscription with no group
// setting is <service>.<subscription>.
func CheckSettings(service string, subscriptions []string) error {
	if err := CheckFormer(subscriptions); err != nil {
		return err
	}
	groups := map[string]string{}
	for _, subscription := range subscriptions {
		if os.Getenv(kafkaoutbox.SubscriptionSetting(SubscriptionTopicSetting, subscription)) == "" && os.Getenv(kafkaoutbox.SubscriptionSetting(SubscriptionTopicFileSetting, subscription)) == "" {
			return &SettingError{Variable: Setting(SubscriptionTopicSetting, subscription),
				Problem: "or " + Setting(SubscriptionTopicFileSetting, subscription) + " must be set for the worker mode: it is the topic of the subscription " + subscription}
		}
		_, group, err := kafkaoutbox.ReadSubscriptionConsumer(subscription)
		if err != nil {
			// The error of kafkaoutbox names its setting, the topic or the group setting or its _FILE form, and holds
			// no value (security review of PR #483, L1).
			return readError(subscription, err)
		}
		if group == "" {
			group = service + "." + subscription
		}
		if first, taken := groups[group]; taken {
			return groupClash(subscription, first)
		}
		groups[group] = subscription
	}
	return nil
}

// readError is the refusal of CheckSettings for an error of kafkaoutbox.ReadSubscriptionConsumer: a SettingError
// that names the setting that kafkaoutbox names, which may be the topic or the group setting or its _FILE form, and
// holds no value of it. Any other error gives a fixed SettingError that names the topic and the group setting of the
// subscription, and holds no error text (security review of PR #486, I1).
func readError(subscription string, err error) error {
	var setting *kafkaoutbox.SettingError
	if errors.As(err, &setting) {
		return &SettingError{Variable: setting.Variable, Problem: setting.Problem}
	}
	return &SettingError{Variable: Setting(SubscriptionTopicSetting, subscription) + " or " + Setting(SubscriptionGroupSetting, subscription),
		Problem: "of the subscription " + subscription + " cannot be read"}
}

// groupClash is the refusal of the subscription subscription whose consumer group is the group of the subscription
// first: it names the group setting of subscription and no group.
func groupClash(subscription, first string) error {
	return &SettingError{Variable: Setting(SubscriptionGroupSetting, subscription),
		Problem: "gives the subscription " + subscription + " the consumer group of the subscription " + first + "; give each subscription a group of its own, because one group splits the partitions between the two and each loses records"}
}

// ReadSettings reads the settings of the inbox of the subscription subscription of the service service from the
// process environment: the connection settings as the Kafka destination reads them, the topic, which must be set,
// and the consumer group, which is <service>.<subscription> when it is not set, each by the name of the subscription
// (ST-SUP-RP01-LC38). It refuses a value of a setting of kafka-consumer before 0.5.0 and a setting that it cannot use
// with an error that names the setting and holds no value of it.
func ReadSettings(service, subscription string) (Settings, error) {
	if err := CheckFormer([]string{subscription}); err != nil {
		return Settings{}, err
	}
	connection, err := kafkaoutbox.ReadSettings()
	if err != nil {
		return Settings{}, err
	}
	connection.Topic = ""
	s := Settings{Subscription: subscription, Connection: connection}
	if s.Topic, s.Group, err = kafkaoutbox.ReadSubscriptionConsumer(subscription); err != nil {
		return Settings{}, err
	}
	if s.Group == "" {
		s.Group = service + "." + subscription
	}
	switch os.Getenv(kafkaoutbox.SubscriptionSetting(SubscriptionInfiniteRetentionSetting, subscription)) {
	case "", "0":
	case "1":
		s.AcceptInfiniteRetention = true
	default:
		return Settings{}, &SettingError{Variable: Setting(SubscriptionInfiniteRetentionSetting, subscription), Problem: "must be 0 or 1"}
	}
	return s, s.Check()
}

// Check refuses settings that the inbox cannot use: connection settings that the Kafka destination refuses, no
// topic, a topic that Kafka does not take or keeps for itself, and a group that is not a name of letters, digits,
// '.', '_' and '-'. Its error names the setting of the subscription.
func (s Settings) Check() error {
	if err := s.Connection.Check(); err != nil {
		return err
	}
	topic, group := Setting(SubscriptionTopicSetting, s.Subscription), Setting(SubscriptionGroupSetting, s.Subscription)
	switch {
	case s.Topic == "":
		return &SettingError{Variable: topic, Problem: "or " + Setting(SubscriptionTopicFileSetting, s.Subscription) + " must be set for the worker mode: it is the topic of the subscription " + s.Subscription}
	case !topicPattern.MatchString(s.Topic) || s.Topic == "." || s.Topic == ".." || strings.HasPrefix(s.Topic, "__"):
		return &SettingError{Variable: topic, Problem: "must be a Kafka topic name that Kafka does not keep for itself"}
	case !groupPattern.MatchString(s.Group):
		return &SettingError{Variable: group, Problem: "must be a group name of 1 to 249 letters, digits, '.', '_' or '-'"}
	}
	return nil
}

// CheckGroups refuses two inboxes of inboxes with the same consumer group, with an error that names the group setting
// of the second: one group would split the partitions of the topics between the two, so each would lose records
// (ST-SUP-RP14-LC08, decision on ST-SUP-Q204). It holds no value of a setting.
func CheckGroups(inboxes []*Inbox) error {
	seen := map[string]string{}
	for _, in := range inboxes {
		if first, taken := seen[in.settings.Group]; taken {
			return groupClash(in.sub.Name, first)
		}
		seen[in.settings.Group] = in.sub.Name
	}
	return nil
}
