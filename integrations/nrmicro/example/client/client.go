// Copyright 2020 New Relic Corporation. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/dingyalin/pinpoint-go-agent/integrations/nrmicro"
	proto "github.com/dingyalin/pinpoint-go-agent/integrations/nrmicro/example/proto"
	pinpoint "github.com/dingyalin/pinpoint-go-agent/pinpoint"
	"github.com/micro/go-micro"
)

func main() {
	app, err := pinpoint.NewApplication(
		pinpoint.ConfigAppName("GoMicroClientDemo"),
		pinpoint.ConfigAgentID("GoMicroClientDemo"),
		//pinpoint.ConfigEnabled(false),
		pinpoint.ConfigCollectorUploaded(false),
		//pinpoint.ConfigCollectorUploadedAgentStat(false),
		pinpoint.ConfigCollectorIP("127.0.0.1"),
		pinpoint.ConfigCollectorTCPPort(9994),
		pinpoint.ConfigCollectorStatPort(9995),
		pinpoint.ConfigCollectorSpanPort(9996),
		pinpoint.ConfigDebugLogger(os.Stdout),
	)
	if nil != err {
		panic(err)
	}
	err = app.WaitForConnection(10 * time.Second)
	if nil != err {
		panic(err)
	}
	defer app.Shutdown(10 * time.Second)

	txn := app.StartTransaction("client")
	defer txn.End()

	service := micro.NewService(
		// Add the New Relic wrapper to the client which will create External
		// segments for each out going call.
		micro.WrapClient(nrmicro.ClientWrapper()),
	)
	service.Init()
	ctx := pinpoint.NewContext(context.Background(), txn)
	c := proto.NewGreeterService("greeter", service.Client())

	rsp, err := c.Hello(ctx, &proto.HelloRequest{
		Name: "John",
	})
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(rsp.Greeting)

	time.Sleep(10 * time.Second)
}
