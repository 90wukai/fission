/*
Copyright 2016 The Fission Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package messageQueue

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"

	ns "github.com/nats-io/go-nats-streaming"
	nsUtil "github.com/nats-io/nats-streaming-server/util"
	log "github.com/sirupsen/logrus"

	"github.com/fission/fission"
	"github.com/fission/fission/crd"
)

const (
	natsClusterID  = "fissionMQTrigger"
	natsProtocol   = "nats://"
	natsClientID   = "fission"
	natsQueueGroup = "fission-messageQueueNatsTrigger"
)

type (
	Nats struct {
		nsConn    ns.Conn
		routerUrl string
	}
)

func makeNatsMessageQueue(routerUrl string, mqCfg MessageQueueConfig) (MessageQueue, error) {
	conn, err := ns.Connect(natsClusterID, natsClientID, ns.NatsURL(mqCfg.Url))
	if err != nil {
		return nil, err
	}
	nats := Nats{
		nsConn:    conn,
		routerUrl: routerUrl,
	}
	return nats, nil
}

func (nats Nats) subscribe(trigger *crd.MessageQueueTrigger) (messageQueueSubscription, error) {
	subj := trigger.Spec.Topic

	if !isTopicValidForNats(subj) {
		return nil, errors.New(fmt.Sprintf("Not a valid topic: %s", trigger.Spec.Topic))
	}

	opts := []ns.SubscriptionOption{
		// Create a durable subscription to nats, so that triggers could retrieve last unack message.
		// https://github.com/nats-io/go-nats-streaming#durable-subscriptions
		ns.DurableName(string(trigger.Metadata.UID)),

		// Nats-streaming server is auto-ack mode by default. Since we want nats-streaming server to
		// resend a message if the trigger does not ack it, we need to enable the manual ack mode, so that
		// trigger could choose to ack message or simply drop it depend on the response of function pod.
		ns.SetManualAckMode(),
	}
	sub, err := nats.nsConn.Subscribe(subj, msgHandler(&nats, trigger), opts...)
	if err != nil {
		return nil, err
	}
	return sub, nil
}

func (nats Nats) unsubscribe(subscription messageQueueSubscription) error {
	return subscription.(ns.Subscription).Close()
}

func isTopicValidForNats(topic string) bool {
	// nats-streaming does not support wildcard channel.
	return nsUtil.IsChannelNameValid(topic, false)
}

func msgHandler(nats *Nats, trigger *crd.MessageQueueTrigger) func(*ns.Msg) {
	return func(msg *ns.Msg) {

		// Support other function ref types
		if trigger.Spec.FunctionReference.Type != fission.FunctionReferenceTypeFunctionName {
			log.Fatalf("Unsupported function reference type (%v) for trigger %v",
				trigger.Spec.FunctionReference.Type, trigger.Metadata.Name)
		}

		url := nats.routerUrl + "/" + strings.TrimPrefix(fission.UrlForFunction(trigger.Spec.FunctionReference.Name), "/")
		log.Printf("Making HTTP request to %v", url)

		headers := map[string]string{
			"X-Fission-MQTrigger-Topic":      trigger.Spec.Topic,
			"X-Fission-MQTrigger-RespTopic":  trigger.Spec.ResponseTopic,
			"X-Fission-MQTrigger-ErrorTopic": trigger.Spec.ErrorTopic,
			"Content-Type":                   trigger.Spec.ContentType,
		}

		log.Info("Making sure the NATS message handler recognizes a valid error topic: ", trigger.Spec.ErrorTopic)
		log.Info("And max retries value: ", trigger.Spec.MaxRetries)

		// Create request
		req, err := http.NewRequest("POST", url, bytes.NewReader(msg.Data))

		if err != nil {
			log.Errorf("Could not issue POST request with message to url %v", url)
			return
		}

		for k, v := range headers {
			req.Header.Add(k, v)
		}

		/*
			Cases:
				HTTP response is nil 							-> Retry if within max retries limit, else return
				HTTP response body could not be read 			-> Return
				HTTP request returned error or non-200 status	-> Publish error to error queue if specified and return
				HTTP request did not return error and 200 status-> Ack the message and publish response to resp topic
		*/

		var resp *http.Response
		// Number of retries is required to be between 1 and 5, inclusive
		for attempt := 0; attempt < trigger.Spec.MaxRetries; attempt++ {
			// Make the request
			log.Warningf("Request : %v", req)
			resp, err = http.DefaultClient.Do(req)
			if resp == nil {
				// Retry without referencing status code of nil response on the next line
				continue
			}
			if err == nil && resp.StatusCode == 200 {
				// Success, quit retries
				break
			}
		}

		// Where should the following line go?
		defer resp.Body.Close()

		if resp == nil {
			log.Warning("The response was undefined. Quit.")
			return
		}

		body, bodyErr := ioutil.ReadAll(resp.Body)
		if bodyErr != nil {
			log.Warningf("Response body error: %v", string(body))
			return
		}

		// Only the latest error response will be published to error topic
		if err != nil || resp.StatusCode != 200 {
			log.Errorf("Request to %v failed after %v retries, err : %v", url, trigger.Spec.MaxRetries, err)
			log.Info("Attempting to publish error to error queue, if defined.")
			log.Info("The response body is: %v", body)

			if len(trigger.Spec.ErrorTopic) > 0 {
				publishErr := nats.nsConn.Publish(trigger.Spec.ErrorTopic, body)
				if publishErr != nil {
					log.Error("Failed to publish error to error topic: %v", err)
				}
			}
			return
		}

		// trigger acks message only if a request done successfully
		err = msg.Ack()
		if err != nil {
			log.Warningf("Failed to ack message: %v", err)
		}

		if len(trigger.Spec.ResponseTopic) > 0 {
			err = nats.nsConn.Publish(trigger.Spec.ResponseTopic, body)
			if err != nil {
				log.Warningf("Failed to publish message to topic %s: %v", trigger.Spec.ResponseTopic, err)
			}
		}
	}

}
