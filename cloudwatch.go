package zapcloudwatch

import (
	"container/list"
	"encoding/json"
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/cloudwatchlogs"
	"go.uber.org/zap/zapcore"
	"sync"
	"time"
)

// CloudwatchHook is a zap Hook for dispatching messages to the specified
type CloudwatchHook struct {
	// Messages with a log level not contained in this array
	// will not be dispatched. If nil, all messages will be dispatched.
	AcceptedLevels    []zapcore.Level
	GroupName         string
	StreamName        string
	AWSConfig         *aws.Config
	nextSequenceToken *string
	svc               *cloudwatchlogs.CloudWatchLogs
	Async             bool // if async is true, send a message asynchronously.
	m                 sync.Mutex
}

type PikaCore struct {
	zapcore.Core
}

type EntryQueue struct {
	sync.Mutex
	entries *list.List
}

func (eq *EntryQueue) Push(entry zapcore.Entry) {
	eq.Lock()
	defer eq.Unlock()

	eq.entries.PushBack(entry)
}

func (eq *EntryQueue) Pop() *zapcore.Entry {
	eq.Lock()
	defer eq.Unlock()

	if eq.entries.Len() == 0 {
		return nil
	}

	e := eq.entries.Front()
	eq.entries.Remove(e)

	entry := e.Value.(zapcore.Entry)

	return &entry
}

var msgCache = EntryQueue{
	entries: list.New(),
}

func (c *PikaCore) Check(entry zapcore.Entry, checked *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(entry.Level) {
		return checked.AddCore(entry, c)
	}
	return checked
}

func (c *PikaCore) Write(entry zapcore.Entry, fields []zapcore.Field) error {
	// Original conversion of fields to a map.
	fieldsMap := make(map[string]interface{})
	for _, field := range fields {
		switch field.Type {
		case zapcore.StringType:
			fieldsMap[field.Key] = field.String
		case zapcore.Int64Type, zapcore.Int32Type, zapcore.Uint32Type, zapcore.Uint64Type:
			fieldsMap[field.Key] = field.Integer
		case zapcore.BoolType:
			fieldsMap[field.Key] = field.Integer == 1
		default:
			fieldsMap[field.Key] = field.Interface
		}
	}

	fieldsJson, err := json.Marshal(fieldsMap)
	if err != nil {
		return err
	}

	entry.Message = fmt.Sprintf("%s %s", entry.Message, string(fieldsJson))

	msgCache.Push(entry)

	// Continue with your existing code to send the log to AWS CloudWatch.
	return c.Core.Write(entry, fields)
}

// NewCloudwatchHook creates a new zap hook for cloudwatch
func NewCloudwatchHook(groupName, streamName string, isAsync bool, cfg *aws.Config, level zapcore.Level) *CloudwatchHook {
	return &CloudwatchHook{
		GroupName:      groupName,
		StreamName:     streamName,
		AWSConfig:      cfg,
		Async:          isAsync,
		AcceptedLevels: LevelThreshold(level),
	}
}

// GetHook function returns hook to zap
func (ch *CloudwatchHook) GetHook() (func(zapcore.Entry) error, error) {

	var cloudwatchWriter = func(e zapcore.Entry) error {
		if !ch.isAcceptedLevel(e.Level) {
			return nil
		}

		modifiedEntry := msgCache.Pop()
		if modifiedEntry != nil {
			e = *modifiedEntry
		}

		event := &cloudwatchlogs.InputLogEvent{
			Message:   aws.String(fmt.Sprintf("[%s] %s", e.LoggerName, e.Message)),
			Timestamp: aws.Int64(int64(time.Nanosecond) * time.Now().UnixNano() / int64(time.Millisecond)),
		}
		params := &cloudwatchlogs.PutLogEventsInput{
			LogEvents:     []*cloudwatchlogs.InputLogEvent{event},
			LogGroupName:  aws.String(ch.GroupName),
			LogStreamName: aws.String(ch.StreamName),
			SequenceToken: ch.nextSequenceToken,
		}

		if ch.Async {
			go ch.sendEvent(params)
			return nil
		}

		return ch.sendEvent(params)
	}

	ch.svc = cloudwatchlogs.New(session.New(ch.AWSConfig))

	lgresp, err := ch.svc.DescribeLogGroups(&cloudwatchlogs.DescribeLogGroupsInput{LogGroupNamePrefix: aws.String(ch.GroupName), Limit: aws.Int64(1)})
	if err != nil {
		return nil, err
	}

	if len(lgresp.LogGroups) < 1 {
		// we need to create this log group
		_, err := ch.svc.CreateLogGroup(&cloudwatchlogs.CreateLogGroupInput{LogGroupName: aws.String(ch.GroupName)})
		if err != nil {
			return nil, err
		}
	}

	resp, err := ch.svc.DescribeLogStreams(&cloudwatchlogs.DescribeLogStreamsInput{
		LogGroupName:        aws.String(ch.GroupName), // Required
		LogStreamNamePrefix: aws.String(ch.StreamName),
	})
	if err != nil {
		return nil, err
	}

	// grab the next sequence token
	if len(resp.LogStreams) > 0 {
		ch.nextSequenceToken = resp.LogStreams[0].UploadSequenceToken
		return cloudwatchWriter, nil
	}

	// create stream if it doesn't exist. the next sequence token will be null
	_, err = ch.svc.CreateLogStream(&cloudwatchlogs.CreateLogStreamInput{
		LogGroupName:  aws.String(ch.GroupName),
		LogStreamName: aws.String(ch.StreamName),
	})

	if err != nil {
		return nil, err
	}
	return cloudwatchWriter, nil
}

func (ch *CloudwatchHook) sendEvent(params *cloudwatchlogs.PutLogEventsInput) error {

	ch.m.Lock()
	defer ch.m.Unlock()

	resp, err := ch.svc.PutLogEvents(params)
	if err != nil {
		return err
	}
	ch.nextSequenceToken = resp.NextSequenceToken
	return nil
}

// Levels sets which levels to sent to cloudwatch
func (ch *CloudwatchHook) Levels() []zapcore.Level {
	if ch.AcceptedLevels == nil {
		return AllLevels
	}
	return ch.AcceptedLevels
}

func (ch *CloudwatchHook) isAcceptedLevel(level zapcore.Level) bool {
	for _, lv := range ch.Levels() {
		if lv == level {
			return true
		}
	}
	return false
}

// AllLevels Supported log levels
var AllLevels = []zapcore.Level{
	zapcore.DebugLevel,
	zapcore.InfoLevel,
	zapcore.WarnLevel,
	zapcore.ErrorLevel,
	zapcore.FatalLevel,
	zapcore.PanicLevel,
}

// LevelThreshold - Returns every logging level above and including the given parameter.
func LevelThreshold(l zapcore.Level) []zapcore.Level {
	for i := range AllLevels {
		if AllLevels[i] == l {
			return AllLevels[i:]
		}
	}
	return []zapcore.Level{}
}
