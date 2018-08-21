// Copyright (c) 2017 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package mocks

import (
	mock "github.com/stretchr/testify/mock"
	"github.com/uber/cadence/.gen/go/replicator"
	"github.com/uber/cadence/common/messaging"
)

// KafkaProducer is an autogenerated mock type for the KafkaProducer type
type KafkaProducer struct {
	mock.Mock
}

// Close provides a mock function with given fields:
func (_m *KafkaProducer) Close() error {
	ret := _m.Called()

	var r0 error
	if rf, ok := ret.Get(0).(func() error); ok {
		r0 = rf()
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// Publish provides a mock function with given fields: msg
func (_m *KafkaProducer) Publish(msg *replicator.ReplicationTask) error {
	ret := _m.Called(msg)

	var r0 error
	if rf, ok := ret.Get(0).(func(*replicator.ReplicationTask) error); ok {
		r0 = rf(msg)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// PublishBatch provides a mock function with given fields: msgs
func (_m *KafkaProducer) PublishBatch(msgs []*replicator.ReplicationTask) error {
	ret := _m.Called(msgs)

	var r0 error
	if rf, ok := ret.Get(0).(func([]*replicator.ReplicationTask) error); ok {
		r0 = rf(msgs)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

var _ messaging.Producer = (*KafkaProducer)(nil)
