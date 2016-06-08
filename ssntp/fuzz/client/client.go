//
// Copyright (c) 2016 Intel Corporation
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//

// +build gofuzz

package ssntpclient

import (
	"bytes"
	"encoding/gob"

	"github.com/01org/ciao/ssntp"
)

type ssntpClient struct {
	ssntp      ssntp.Client
	retChannel chan int
}

func (client *ssntpClient) ConnectNotify() {
}

func (client *ssntpClient) DisconnectNotify() {
}

func (client *ssntpClient) CommandNotify(command ssntp.Command, frame *ssntp.Frame) {
}

func (client *ssntpClient) StatusNotify(status ssntp.Status, frame *ssntp.Frame) {
}

func (client *ssntpClient) EventNotify(event ssntp.Event, frame *ssntp.Frame) {
}

func (client *ssntpClient) ErrorNotify(error ssntp.Error, frame *ssntp.Frame) {
}

func Fuzz(data []byte) int {
	var client ssntpClient
	var buffer bytes.Buffer
	var f ssntp.Frame

	buffer.Write(data)
	dec := gob.NewDecoder(&buffer)

	err := dec.Decode(&f)
	if err != nil {
		return -1
	}

	client.ssntp.FuzzProcessSSNTPFrame(&f)

	return 1
}
