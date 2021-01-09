// Copyright 2020 New Relic Corporation. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pinpoint

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dingyalin/pinpoint-go-agent/internal"
	"github.com/dingyalin/pinpoint-go-agent/internal/cat"
	"github.com/dingyalin/pinpoint-go-agent/thrift/dto/trace"
	"github.com/dingyalin/pinpoint-go-agent/thrift/io"
)

var (
	sequenceID int64
)

func nextSequenceID() int64 {
	return atomic.AddInt64(&sequenceID, 1)
}

// encodeTraceID
// byte(version) + byte(len(agentID)) + []byte(agentID) + varint(startTime) + varint(sequenceID)
func encodeTraceID(agentID string, startTime int64, sequenceID int64) []byte {
	// version
	var version byte = '\x00'
	// agent id
	agentIDLen := len(agentID)
	agentIDLenByte := byte((agentIDLen << 1) ^ (agentIDLen >> 31))
	// start time
	startTimeBuf := make([]byte, binary.MaxVarintLen64)
	startTimeBytes := startTimeBuf[:binary.PutUvarint(startTimeBuf, uint64(startTime))]
	//fmt.Printf("startTimeBytes: %X \n", startTimeBytes)

	// sequence id
	sequenceIDBuf := make([]byte, binary.MaxVarintLen64)
	sequenceIDBytes := sequenceIDBuf[:binary.PutUvarint(sequenceIDBuf, uint64(sequenceID))]
	// fmt.Printf("sequenceIDBytes: % x \n", sequenceIDBytes)

	var buffer bytes.Buffer
	buffer.Write([]byte{version, agentIDLenByte})
	buffer.Write([]byte(agentID))
	buffer.Write(startTimeBytes)
	buffer.Write(sequenceIDBytes)

	return buffer.Bytes()
}

type txn struct {
	app *app
	*appRun

	// This mutex is required since the consumer may call the public API
	// interface functions from different routines.
	sync.Mutex
	// finished indicates whether or not End() has been called.  After
	finished           bool
	numPayloadsCreated uint32
	sampledCalculated  bool

	ignore bool

	// wroteHeader prevents capturing multiple response code errors if the
	// user erroneously calls WriteHeader multiple times.
	wroteHeader bool

	txnData

	mainThread   tracingThread
	asyncThreads []*tracingThread
}

type thread struct {
	*txn
	// thread does not have locking because it should only be accessed while
	// the txn is locked.
	thread *tracingThread
}

func (txn *txn) markStart(now time.Time) {
	txn.Start = now
	// The mainThread is considered active now.
	txn.mainThread.RecordActivity(now)

}

func (txn *txn) markEnd(now time.Time, thread *tracingThread) {
	txn.Stop = now
	// The thread on which End() was called is considered active now.
	thread.RecordActivity(now)
	txn.Duration = txn.Stop.Sub(txn.Start)

	// TotalTime is the sum of "active time" across all threads.  A thread
	// was active when it started the transaction, stopped the transaction,
	// started a segment, or stopped a segment.
	txn.TotalTime = txn.mainThread.TotalTime()
	for _, thd := range txn.asyncThreads {
		txn.TotalTime += thd.TotalTime()
	}
	// Ensure that TotalTime is at least as large as Duration so that the
	// graphs look sensible.  This can happen under the following situation:
	// goroutine1: txn.start----|segment1|
	// goroutine2:                                   |segment2|----txn.end
	if txn.Duration > txn.TotalTime {
		txn.TotalTime = txn.Duration
	}
}

func (txn *txn) nextSpanID() int64 {
	for {
		spanID := genRandSpanID()
		metadata := txn.txnData.CrossProcess.InboundMetadata
		crossPSpanID := metadata.PinpointPspanid
		if spanID > 0 && spanID != txn.SpanID && spanID != crossPSpanID {
			return spanID
		}
	}
}

func genRandSpanID() int64 {
	return int64(rand.Int31())
}

func newTxn(app *app, run *appRun, name string) *thread {
	txn := &txn{
		app:    app,
		appRun: run,
	}
	txn.markStart(time.Now())

	txn.Name = name
	txn.Attrs = newAttributes(run.AttributeConfig)
	txn.SpanID = txn.nextSpanID()
	txn.SequenceID = nextSequenceID()
	txn.TraceID = strings.Join([]string{app.config.AgentID, strconv.FormatInt(app.startTime, 10), strconv.FormatInt(txn.SequenceID, 10)}, "^")
	txn.TraceIDEncoded = encodeTraceID(app.config.AgentID, app.startTime, txn.SequenceID)

	if run.Config.DistributedTracer.Enabled {
		txn.BetterCAT.Enabled = true
		txn.TraceIDGenerator = run.Reply.TraceIDGenerator
		txn.BetterCAT.SetTraceAndTxnIDs(txn.TraceIDGenerator.GenerateTraceID())
		txn.BetterCAT.Priority = newPriorityFromRandom(txn.TraceIDGenerator.Float32)
		txn.ShouldCollectSpanEvents = txn.shouldCollectSpanEvents
		txn.ShouldCreateSpanGUID = txn.shouldCreateSpanGUID
	}

	txn.Attrs.Agent.Add(AttributeHostDisplayName, txn.Config.HostDisplayName, nil)
	txn.TxnTrace.Enabled = txn.Config.TransactionTracer.Enabled
	txn.TxnTrace.SegmentThreshold = txn.Config.TransactionTracer.Segments.Threshold
	txn.TxnTrace.StackTraceThreshold = txn.Config.TransactionTracer.Segments.StackTraceThreshold
	txn.SlowQueriesEnabled = txn.Config.DatastoreTracer.SlowQuery.Enabled
	txn.SlowQueryThreshold = txn.Config.DatastoreTracer.SlowQuery.Threshold

	// Synthetics support is tied up with a transaction's Old CAT field,
	// CrossProcess. To support Synthetics with either BetterCAT or Old CAT,
	// Initialize the CrossProcess field of the transaction, passing in
	// the top-level configuration.
	doOldCAT := txn.Config.CrossApplicationTracer.Enabled
	noGUID := txn.Config.DistributedTracer.Enabled
	txn.CrossProcess.Init(doOldCAT, noGUID, run.Reply)

	return &thread{
		txn:    txn,
		thread: &txn.mainThread,
	}
}

func (thd *thread) logAPIError(err error, operation string, extraDetails map[string]interface{}) {
	if nil == thd {
		return
	}
	if nil == err {
		return
	}
	if extraDetails == nil {
		extraDetails = make(map[string]interface{}, 1)
	}
	extraDetails["reason"] = err.Error()
	thd.Config.Logger.Error("unable to "+operation, extraDetails)
}

func (txn *txn) shouldCollectSpanEvents() bool {
	if !txn.Config.DistributedTracer.Enabled {
		return false
	}
	if !txn.Config.SpanEvents.Enabled {
		return false
	}
	if shouldUseTraceObserver(txn.Config) {
		return true
	}
	return txn.lazilyCalculateSampled()
}

func (txn *txn) shouldCreateSpanGUID() bool {
	if !txn.Config.DistributedTracer.Enabled {
		return false
	}
	if !txn.Config.SpanEvents.Enabled {
		return false
	}
	return true
}

// lazilyCalculateSampled calculates and returns whether or not the transaction
// should be sampled.  Sampled is not computed at the beginning of the
// transaction because we want to calculate Sampled only for transactions that
// do not accept an inbound payload.
func (txn *txn) lazilyCalculateSampled() bool {
	if !txn.BetterCAT.Enabled {
		return false
	}
	if txn.sampledCalculated {
		return txn.BetterCAT.Sampled
	}
	txn.BetterCAT.Sampled = txn.appRun.adaptiveSampler.computeSampled(txn.BetterCAT.Priority.Float32(), time.Now())
	if txn.BetterCAT.Sampled {
		txn.BetterCAT.Priority += 1.0
	}
	txn.sampledCalculated = true
	return txn.BetterCAT.Sampled
}

func (txn *txn) SetWebRequest(r WebRequest) error {
	txn.Lock()
	defer txn.Unlock()

	if txn.finished {
		return errAlreadyEnded
	}

	// Any call to SetWebRequest should indicate a web transaction.
	txn.IsWeb = true

	h := r.Header
	if nil != h {
		txn.Queuing = queueDuration(h, txn.Start)
		// txn.acceptDistributedTraceHeadersLocked(r.Transport, h)
		txn.CrossProcess.InboundHTTPRequest(h)
	}

	// cross process
	metadata := txn.CrossProcess.InboundMetadata
	if metadata.PinpointTraceid != "" {
		txn.TraceID = metadata.PinpointTraceid
		txn.TraceIDEncoded = metadata.PinpointTraceidEncoded
		txn.SpanID = metadata.PinpointSpanid
	}

	requestAgentAttributes(txn.Attrs, r.Method, h, r.URL, r.Host)

	return nil
}

type dummyResponseWriter struct{}

func (rw dummyResponseWriter) Header() http.Header { return nil }

func (rw dummyResponseWriter) Write(b []byte) (int, error) { return 0, nil }

func (rw dummyResponseWriter) WriteHeader(code int) {}

func (thd *thread) SetWebResponse(w http.ResponseWriter) http.ResponseWriter {
	txn := thd.txn
	txn.Lock()
	defer txn.Unlock()

	if w == nil {
		// Accepting a nil parameter makes it easy for consumers to add
		// a response code to the transaction without a response
		// writer:
		//
		//    txn.SetWebResponse(nil).WriteHeader(500)
		//
		w = dummyResponseWriter{}
	}

	return upgradeResponseWriter(&replacementResponseWriter{
		thd:      thd,
		original: w,
	})
}

func (txn *txn) freezeName() {
	if txn.ignore || ("" != txn.FinalName) {
		return
	}
	txn.FinalName = txn.appRun.createTransactionName(txn.Name, txn.IsWeb)
	if "" == txn.FinalName {
		txn.ignore = true
	}
}

func (txn *txn) getsApdex() bool {
	return txn.IsWeb
}

func (txn *txn) shouldSaveTrace() bool {
	if !txn.Config.TransactionTracer.Enabled {
		return false
	}
	if txn.CrossProcess.IsSynthetics() {
		return true
	}
	return txn.Duration >= txn.txnTraceThreshold(txn.ApdexThreshold)
}

func (txn *txn) MergeIntoHarvest(h *harvest) {
	/*
		var priority priority
		if txn.BetterCAT.Enabled {
			priority = txn.BetterCAT.Priority
		} else {
			priority = newPriority()
		}

		createTxnMetrics(&txn.txnData, h.Metrics)
		mergeBreakdownMetrics(&txn.txnData, h.Metrics)

		if txn.Config.TransactionEvents.Enabled {
			// Allocate a new TxnEvent to prevent a reference to the large transaction.
			alloc := new(txnEvent)
			*alloc = txn.txnData.txnEvent
			h.TxnEvents.AddTxnEvent(alloc, priority)
		}

		if txn.Reply.CollectErrors {
			mergeTxnErrors(&h.ErrorTraces, txn.Errors, txn.txnEvent)
		}

		if txn.Config.ErrorCollector.CaptureEvents {
			for _, e := range txn.Errors {
				errEvent := &errorEvent{
					errorData: *e,
					txnEvent:  txn.txnEvent,
				}
				// Since the stack trace is not used in error events, remove the reference
				// to minimize memory.
				errEvent.Stack = nil
				h.ErrorEvents.Add(errEvent, priority)
			}
		}

		if txn.shouldSaveTrace() {
			h.TxnTraces.Witness(harvestTrace{
				txnEvent: txn.txnEvent,
				Trace:    txn.TxnTrace,
			})
		}

		if nil != txn.SlowQueries {
			h.SlowSQLs.Merge(txn.SlowQueries, txn.txnEvent)
		}

		if txn.shouldCollectSpanEvents() && !shouldUseTraceObserver(txn.Config) {
			h.SpanEvents.MergeSpanEvents(txn.txnData.SpanEvents)
		}
	*/

	if txn.IsAsync {
		spanChunk := txn.toAsyncTSpanChunk()
		err := txn.app.pinpointClient.SendSpanChunk(spanChunk)
		if err != nil {
			txn.app.Warn("SendSpanChunk failed", map[string]interface{}{
				"err": err,
			})
		}
	} else {
		tspan := txn.toTSpan()
		err := txn.app.pinpointClient.SendSpan(tspan)
		if err != nil {
			txn.app.Warn("SendSpan failed", map[string]interface{}{
				"err": err,
			})
		}
	}

}

func headersJustWritten(thd *thread, code int, hdr http.Header) {
	txn := thd.txn
	txn.Lock()
	defer txn.Unlock()

	if txn.finished {
		return
	}
	if txn.wroteHeader {
		return
	}
	txn.wroteHeader = true

	responseHeaderAttributes(txn.Attrs, hdr)
	responseCodeAttribute(txn.Attrs, code)

	if txn.appRun.responseCodeIsError(code) {
		e := txnErrorFromResponseCode(time.Now(), code)
		e.Stack = getStackTrace()
		thd.noticeErrorInternal(e)
	}
}

func (txn *txn) responseHeader(hdr http.Header) http.Header {
	txn.Lock()
	defer txn.Unlock()

	if txn.finished {
		return nil
	}
	if txn.wroteHeader {
		return nil
	}
	if !txn.CrossProcess.Enabled {
		return nil
	}
	if !txn.CrossProcess.IsInbound() {
		return nil
	}
	txn.freezeName()
	contentLength := getContentLengthFromHeader(hdr)

	appData, err := txn.CrossProcess.CreateAppData(txn.FinalName, txn.Queuing, time.Since(txn.Start), contentLength)
	if err != nil {
		txn.Config.Logger.Debug("error generating outbound response header", map[string]interface{}{
			"error": err,
		})
		return nil
	}
	return appDataToHTTPHeader(appData)
}

func addCrossProcessHeaders(txn *txn, hdr http.Header) {
	// responseHeader() checks the wroteHeader field and returns a nil map if the
	// header has been written, so we don't need a check here.
	if nil != hdr {
		for key, values := range txn.responseHeader(hdr) {
			for _, value := range values {
				hdr.Add(key, value)
			}
		}
	}
}

func (thd *thread) End(recovered interface{}) error {
	txn := thd.txn
	txn.Lock()
	defer txn.Unlock()

	if txn.finished {
		return errAlreadyEnded
	}

	txn.finished = true

	if nil != recovered {
		e := txnErrorFromPanic(time.Now(), recovered)
		e.Stack = getStackTrace()
		thd.noticeErrorInternal(e)
	}

	txn.markEnd(time.Now(), thd.thread)
	txn.freezeName()
	// Make a sampling decision if there have been no segments or outbound
	// payloads.
	txn.lazilyCalculateSampled()

	// Finalise the CAT state.
	if err := txn.CrossProcess.Finalise(txn.Name, txn.Config.AppName); err != nil {
		txn.Config.Logger.Debug("error finalising the cross process state", map[string]interface{}{
			"error": err,
		})
	}

	// Assign apdexThreshold regardless of whether or not the transaction
	// gets apdex since it may be used to calculate the trace threshold.
	txn.ApdexThreshold = internal.CalculateApdexThreshold(txn.Reply, txn.FinalName)

	if txn.getsApdex() {
		if txn.HasErrors() {
			txn.Zone = apdexFailing
		} else {
			txn.Zone = calculateApdexZone(txn.ApdexThreshold, txn.Duration)
		}
	} else {
		txn.Zone = apdexNone
	}

	if txn.Config.Logger.DebugEnabled() {
		txn.Config.Logger.Debug("transaction ended", map[string]interface{}{
			"name":          txn.FinalName,
			"duration_ms":   txn.Duration.Seconds() * 1000.0,
			"ignored":       txn.ignore,
			"app_connected": "" != txn.Reply.RunID,
		})
	}

	/*
		if txn.shouldCollectSpanEvents() {
			root := &spanEvent{
				GUID:         txn.GetRootSpanID(),
				Timestamp:    txn.Start,
				Duration:     txn.Duration,
				Name:         txn.FinalName,
				TxnName:      txn.FinalName,
				Category:     spanCategoryGeneric,
				IsEntrypoint: true,
			}
			root.AgentAttributes.addAgentAttrs(txn.Attrs.Agent)
			root.UserAttributes.addUserAttrs(txn.Attrs.user)

			if txn.rootSpanErrData != nil {
				root.AgentAttributes.addString(SpanAttributeErrorClass, txn.rootSpanErrData.Klass)
				root.AgentAttributes.addString(SpanAttributeErrorMessage, txn.rootSpanErrData.Msg)
			}
			if p := txn.BetterCAT.Inbound; nil != p {
				root.ParentID = txn.BetterCAT.Inbound.ID
				root.TrustedParentID = txn.BetterCAT.Inbound.TrustedParentID
				root.TracingVendors = txn.BetterCAT.Inbound.TracingVendors
				if p.HasNewRelicTraceInfo {
					root.AgentAttributes.addString("parent.type", p.Type)
					root.AgentAttributes.addString("parent.app", p.App)
					root.AgentAttributes.addString("parent.account", p.Account)
					root.AgentAttributes.addFloat("parent.transportDuration", p.TransportDuration.Seconds())
				}
				root.AgentAttributes.addString("parent.transportType", txn.BetterCAT.TransportType)
			}
			root.AgentAttributes = txn.Attrs.filterSpanAttributes(root.AgentAttributes, destSpan)
			txn.SpanEvents = append(txn.SpanEvents, root)

			// Add transaction tracing fields to span events at the end of
			// the transaction since we could accept payload after the early
			// segments occur.
			for _, evt := range txn.SpanEvents {
				evt.TraceID = txn.BetterCAT.TraceID
				evt.TransactionID = txn.BetterCAT.TxnID
				evt.Sampled = txn.BetterCAT.Sampled
				evt.Priority = txn.BetterCAT.Priority
			}
		}
	*/

	if !txn.ignore {
		txn.app.Consume(txn.Reply.RunID, txn)
		/*
			if observer := txn.app.getObserver(); nil != observer {
				for _, evt := range txn.SpanEvents {
					observer.consumeSpan(evt)
				}
			}
		*/
	}

	// Note that if a consumer uses `panic(nil)`, the panic will not
	// propagate.
	if nil != recovered {
		panic(recovered)
	}

	return nil
}

func (txn *txn) getTSpanEventList() []*trace.TSpanEvent {
	config := txn.Config
	var asyncID *int32
	var asyncSequence *int16
	if txn.IsAsync {
		asyncID = &txn.AsyncID
		asyncSequence = &txn.AsyncSequence
	}

	tSpanEventList := []*trace.TSpanEvent{}
	for _, evt := range txn.SpanEvents {
		if evt.IsEntrypoint {
			// skip entry point
			continue
		}

		nextSpanID := evt.nextSpanID
		if nextSpanID == 0 {
			nextSpanID = -1
		}

		// annotations
		annotations := []*trace.TAnnotation{}
		var serviceType int16 = io.ServiceTypeGoMethod
		if nextSpanID != -1 {
			serviceType = io.ServiceTypeHTTPClient
		}
		// rpc
		if evt.rpc != nil {
			annotations = append(annotations, &trace.TAnnotation{
				Key: io.TAnnotationHTTPUrl,
				Value: &trace.TAnnotationValue{
					StringValue: evt.rpc,
				},
			})
		}
		// status code
		if evt.statusCode != nil {
			intValue := int32(*evt.statusCode)
			annotations = append(annotations, &trace.TAnnotation{
				Key: io.TAnnotationHTTPStatusCode,
				Value: &trace.TAnnotationValue{
					IntValue: &intValue,
				},
			})
		}
		// sql

		tSpanEvent := &trace.TSpanEvent{
			Sequence:      evt.segmentStartTime.Sequence,         // from 1
			Depth:         int32(evt.segmentStartTime.Depth + 2), // from 2
			ApiId:         txn.getAPIID(evt.Name),
			ServiceType:   serviceType,
			StartElapsed:  int32(evt.Timestamp.Sub(txn.Start).Milliseconds()),
			Annotations:   annotations, //
			DestinationId: evt.destinationID,
			EndPoint:      evt.endPoint,
			NextSpanId:    nextSpanID,
			NextAsyncId:   evt.nextAsyncID,
			RPC:           evt.rpc,
			EndElapsed:    int32(evt.Duration.Milliseconds()), // elaped time
			AsyncId:       asyncID,
			AsyncSequence: asyncSequence,
		}
		config.Logger.Debug("TSpanEvent", map[string]interface{}{
			"TSpanEvent": fmt.Sprintf("%#v", tSpanEvent),
		})
		tSpanEventList = append(tSpanEventList, tSpanEvent)
	}

	root := &trace.TSpanEvent{
		Sequence:      0,
		Depth:         1,
		ApiId:         txn.getAPIID(txn.Name),
		ServiceType:   io.ServiceTypeGoMethod,
		StartElapsed:  0,
		Annotations:   nil,
		DestinationId: nil,
		EndPoint:      nil,
		NextSpanId:    -1,
		NextAsyncId:   nil,
		RPC:           nil,
		EndElapsed:    int32(txn.Duration.Milliseconds()),
		AsyncId:       asyncID,
		AsyncSequence: asyncSequence,
	}
	tSpanEventList = append(tSpanEventList, root)

	return tSpanEventList
}

func (txn *txn) getRPC() *string {
	agentAttributeValue, ok := txn.Attrs.Agent["request.uri"]
	if ok {
		return &agentAttributeValue.stringVal
	}

	return nil
}

func (txn *txn) getAcceptorHost() *string {
	agentAttributeValue, ok := txn.Attrs.Agent[AttributeRequestHost]
	if ok {
		return &agentAttributeValue.stringVal
	}

	return nil
}

func (txn *txn) getAnnotationsErr() (annotations []*trace.TAnnotation, err int32) {
	agentAttributeValue, ok := txn.Attrs.Agent[AttributeResponseCode]
	if ok {
		statusCode, ok := agentAttributeValue.otherVal.(int)
		if ok {
			intValue := int32(statusCode)
			annotations = append(annotations, &trace.TAnnotation{
				Key: io.TAnnotationHTTPStatusCode,
				Value: &trace.TAnnotationValue{
					IntValue: &intValue,
				},
			})

			if statusCode >= 500 {
				err = 1
			}
		}
	}

	for _, errData := range txn.Errors {
		annotations = append(annotations, &trace.TAnnotation{
			Key: io.TAnnotationException,
			Value: &trace.TAnnotationValue{
				StringValue: &errData.Msg,
			},
		})
		err = 1
	}

	return
}

func (txn *txn) getParentApplication() (pAppName *string, pAppType *int16, pSpanID int64) {
	pSpanID = -1

	metadata := txn.txnData.CrossProcess.InboundMetadata
	if metadata.PinpointPappname == "" {
		return
	}
	pAppName = &metadata.PinpointPappname
	pAppType = &metadata.PinpointPapptype
	pSpanID = metadata.PinpointPspanid
	return
}

func (txn *txn) getAPIID(apiName string) *int32 {
	if txn.app.apiMetaDataMap == nil {
		txn.app.apiMetaDataMap = make(map[string]int32)
	}
	if apiID, ok := txn.app.apiMetaDataMap[apiName]; ok {
		return &apiID
	}

	apiID := txn.app.apiID + 1 // from 1
	if len(txn.app.apiMetaDataMap) > txn.Config.APIMetaDataMapSize {
		txn.app.apiMetaDataMap = make(map[string]int32)
	}

	config := txn.Config
	tapiMetaData := &trace.TApiMetaData{
		AgentId:        config.AgentID,
		AgentStartTime: txn.app.startTime,
		ApiId:          apiID,
		ApiInfo:        apiName,
	}
	err := txn.app.pinpointClient.SendTCPTStruct(io.TTypeAPIMetadata, tapiMetaData)
	if err != nil {
		txn.app.Warn("SendTApiMetaData failed", map[string]interface{}{
			"err": err,
		})
		time.Sleep(time.Second)
		return nil
	}

	txn.app.apiMetaDataMap[apiName] = apiID
	txn.app.apiID++
	config.Logger.Debug("TApiMetaData", map[string]interface{}{
		"TApiMetaData": fmt.Sprintf("%#v", tapiMetaData),
	})
	return &apiID
}

func (txn *txn) toAsyncTSpanChunk() *trace.TSpanChunk {
	config := txn.Config
	tspanChunk := &trace.TSpanChunk{
		AgentId:                config.AgentID,
		ApplicationName:        config.AppName,
		AgentStartTime:         txn.app.startTime,
		ServiceType:            io.ServiceTypeGoAsyncMethod,
		TransactionId:          txn.TraceIDEncoded,
		SpanId:                 txn.SpanID,
		EndPoint:               nil, // nil
		SpanEventList:          txn.getTSpanEventList(),
		ApplicationServiceType: &config.ServiceType,
	}

	config.Logger.Debug("TSpanChunk", map[string]interface{}{
		"TSpanChunk": fmt.Sprintf("%#v", tspanChunk),
		//"txn.Attrs.Agent": fmt.Sprintf("%#v", txn.Attrs.Agent),
	})

	return tspanChunk
}

func (txn *txn) toTSpan() *trace.TSpan {
	config := txn.Config
	annotations, err := txn.getAnnotationsErr()
	pAppName, pAppType, pSpanID := txn.getParentApplication()

	tspan := &trace.TSpan{
		AgentId:                config.AgentID,
		ApplicationName:        config.AppName,
		AgentStartTime:         txn.app.startTime,
		TransactionId:          txn.TraceIDEncoded,
		SpanId:                 txn.SpanID,
		ParentSpanId:           pSpanID,
		StartTime:              txn.Start.UnixNano() / 1e6,
		Elapsed:                int32(txn.Duration.Milliseconds()),
		RPC:                    txn.getRPC(),
		ServiceType:            io.ServiceTypeGoMethod,
		EndPoint:               nil, // nil
		RemoteAddr:             nil, // nil
		Annotations:            annotations,
		Flag:                   0,
		Err:                    &err,
		SpanEventList:          txn.getTSpanEventList(),
		ParentApplicationName:  pAppName,
		ParentApplicationType:  pAppType,
		AcceptorHost:           txn.getAcceptorHost(),
		ApiId:                  txn.getAPIID(txn.FinalName),
		ExceptionInfo:          nil, // nil
		ApplicationServiceType: &config.ServiceType,
		LoggingTransactionInfo: nil, // nil
	}

	config.Logger.Debug("TSpan", map[string]interface{}{
		"TSpan": fmt.Sprintf("%#v", tspan),
		//"txn.Attrs.Agent": fmt.Sprintf("%#v", txn.Attrs.Agent),
	})

	return tspan
}

func (txn *txn) AddAttribute(name string, value interface{}) error {
	txn.Lock()
	defer txn.Unlock()

	if txn.Config.HighSecurity {
		return errHighSecurityEnabled
	}

	if !txn.Reply.SecurityPolicies.CustomParameters.Enabled() {
		return errSecurityPolicy
	}

	if txn.finished {
		return errAlreadyEnded
	}

	return addUserAttribute(txn.Attrs, name, value, destAll)
}

var (
	errorsDisabled        = errors.New("errors disabled")
	errNilError           = errors.New("nil error")
	errAlreadyEnded       = errors.New("transaction has already ended")
	errSecurityPolicy     = errors.New("disabled by security policy")
	errTransactionIgnored = errors.New("transaction has been ignored")
	errBrowserDisabled    = errors.New("browser disabled by local configuration")
)

const (
	highSecurityErrorMsg   = "message removed by high security setting"
	securityPolicyErrorMsg = "message removed by security policy"
)

func (thd *thread) noticeErrorInternal(err errorData) error {
	txn := thd.txn
	if !txn.Config.ErrorCollector.Enabled {
		return errorsDisabled
	}

	if nil == txn.Errors {
		txn.Errors = newTxnErrors(maxTxnErrors)
	}

	if txn.Config.HighSecurity {
		err.Msg = highSecurityErrorMsg
	}

	if !txn.Reply.SecurityPolicies.AllowRawExceptionMessages.Enabled() {
		err.Msg = securityPolicyErrorMsg
	}

	if txn.shouldCollectSpanEvents() {
		err.SpanID = txn.CurrentSpanIdentifier(thd.thread)
		addErrorAttrs(thd, err)
	}
	txn.Errors.Add(err)
	txn.txnData.txnEvent.HasError = true //mark transaction as having an error
	return nil
}

var errorAttrs = []string{
	SpanAttributeErrorClass,
	SpanAttributeErrorMessage,
}

func addErrorAttrs(t *thread, err errorData) {
	// If there are no current segments, we'll add them to the root span when it is created later
	if len(t.thread.stack) <= 0 {
		t.rootSpanErrData = &err
		return
	}
	for _, attr := range errorAttrs {
		t.thread.RemoveErrorSpanAttribute(attr)
	}
	t.thread.AddAgentSpanAttribute(SpanAttributeErrorClass, err.Klass)
	t.thread.AddAgentSpanAttribute(SpanAttributeErrorMessage, err.Msg)
}

var (
	errTooManyErrorAttributes = fmt.Errorf("too many extra attributes: limit is %d",
		attributeErrorLimit)
)

// errorCause returns the error's deepest wrapped ancestor.
func errorCause(err error) error {
	for {
		if unwrapper, ok := err.(interface{ Unwrap() error }); ok {
			if next := unwrapper.Unwrap(); nil != next {
				err = next
				continue
			}
		}
		return err
	}
}

func errorClassMethod(err error) string {
	if ec, ok := err.(errorClasser); ok {
		return ec.ErrorClass()
	}
	return ""
}

func errorStackTraceMethod(err error) stackTrace {
	if st, ok := err.(stackTracer); ok {
		return st.StackTrace()
	}
	return nil
}

func errorAttributesMethod(err error) map[string]interface{} {
	if st, ok := err.(errorAttributer); ok {
		return st.ErrorAttributes()
	}
	return nil
}

func errDataFromError(input error) (data errorData, err error) {
	cause := errorCause(input)

	data = errorData{
		When: time.Now(),
		Msg:  input.Error(),
	}

	if c := errorClassMethod(input); "" != c {
		// If the error implements ErrorClasser, use that.
		data.Klass = c
	} else if c := errorClassMethod(cause); "" != c {
		// Otherwise, if the error's cause implements ErrorClasser, use that.
		data.Klass = c
	} else {
		// As a final fallback, use the type of the error's cause.
		data.Klass = reflect.TypeOf(cause).String()
	}

	if st := errorStackTraceMethod(input); nil != st {
		// If the error implements StackTracer, use that.
		data.Stack = st
	} else if st := errorStackTraceMethod(cause); nil != st {
		// Otherwise, if the error's cause implements StackTracer, use that.
		data.Stack = st
	} else {
		// As a final fallback, generate a StackTrace here.
		data.Stack = getStackTrace()
	}

	var unvetted map[string]interface{}
	if ats := errorAttributesMethod(input); nil != ats {
		// If the error implements ErrorAttributer, use that.
		unvetted = ats
	} else {
		// Otherwise, if the error's cause implements ErrorAttributer, use that.
		unvetted = errorAttributesMethod(cause)
	}
	if unvetted != nil {
		if len(unvetted) > attributeErrorLimit {
			err = errTooManyErrorAttributes
			return
		}

		data.ExtraAttributes = make(map[string]interface{})
		for key, val := range unvetted {
			val, err = validateUserAttribute(key, val)
			if nil != err {
				return
			}
			data.ExtraAttributes[key] = val
		}
	}

	return data, nil
}

func (thd *thread) NoticeError(input error) error {
	txn := thd.txn
	txn.Lock()
	defer txn.Unlock()

	if txn.finished {
		return errAlreadyEnded
	}

	if nil == input {
		return errNilError
	}

	data, err := errDataFromError(input)
	if nil != err {
		return err
	}

	if txn.Config.HighSecurity || !txn.Reply.SecurityPolicies.CustomParameters.Enabled() {
		data.ExtraAttributes = nil
	}

	return thd.noticeErrorInternal(data)
}

func (txn *txn) SetName(name string) error {
	txn.Lock()
	defer txn.Unlock()

	if txn.finished {
		return errAlreadyEnded
	}

	txn.Name = name
	return nil
}

func (txn *txn) Ignore() error {
	txn.Lock()
	defer txn.Unlock()

	if txn.finished {
		return errAlreadyEnded
	}
	txn.ignore = true
	return nil
}

func (thd *thread) startSegmentAt(at time.Time) SegmentStartTime {
	var s segmentStartTime
	txn := thd.txn
	txn.Lock()
	if !txn.finished {
		s = startSegment(&txn.txnData, thd.thread, at)
	}
	txn.Unlock()
	return SegmentStartTime{
		start:  s,
		thread: thd,
	}
}

const (
	// Browser fields are encoded using the first digits of the license
	// key.
	browserEncodingKeyLimit = 13
)

func browserEncodingKey(licenseKey string) []byte {
	key := []byte(licenseKey)
	if len(key) > browserEncodingKeyLimit {
		key = key[0:browserEncodingKeyLimit]
	}
	return key
}

func (txn *txn) BrowserTimingHeader() (*BrowserTimingHeader, error) {
	txn.Lock()
	defer txn.Unlock()

	if !txn.Config.BrowserMonitoring.Enabled {
		return nil, errBrowserDisabled
	}

	if txn.Reply.AgentLoader == "" {
		// If the loader is empty, either browser has been disabled
		// by the server or the application is not yet connected.
		return nil, nil
	}

	if txn.finished {
		return nil, errAlreadyEnded
	}

	txn.freezeName()

	// Freezing the name might cause the transaction to be ignored, so check
	// this after txn.freezeName().
	if txn.ignore {
		return nil, errTransactionIgnored
	}

	encodingKey := browserEncodingKey(txn.Config.License)

	attrs, err := obfuscate(browserAttributes(txn.Attrs), encodingKey)
	if err != nil {
		return nil, fmt.Errorf("error getting browser attributes: %v", err)
	}

	name, err := obfuscate([]byte(txn.FinalName), encodingKey)
	if err != nil {
		return nil, fmt.Errorf("error obfuscating name: %v", err)
	}

	return &BrowserTimingHeader{
		agentLoader: txn.Reply.AgentLoader,
		info: browserInfo{
			Beacon:                txn.Reply.Beacon,
			LicenseKey:            txn.Reply.BrowserKey,
			ApplicationID:         txn.Reply.AppID,
			TransactionName:       name,
			QueueTimeMillis:       txn.Queuing.Nanoseconds() / (1000 * 1000),
			ApplicationTimeMillis: time.Now().Sub(txn.Start).Nanoseconds() / (1000 * 1000),
			ObfuscatedAttributes:  attrs,
			ErrorBeacon:           txn.Reply.ErrorBeacon,
			Agent:                 txn.Reply.JSAgentFile,
		},
	}, nil
}

func createThread(txn *txn) *tracingThread {
	newThread := newTracingThread(&txn.txnData)
	txn.asyncThreads = append(txn.asyncThreads, newThread)
	return newThread
}

func (txn *txn) genNextAsyncID() int32 {
	txn.AsyncIDCounter++
	return txn.AsyncIDCounter
}

func (thd *thread) NewGoroutine(name string) *Transaction {
	oldTxn := thd.txn
	oldTxn.Lock()
	defer oldTxn.Unlock()

	newTxn := &txn{
		app:    oldTxn.app,
		appRun: oldTxn.appRun,
	}
	newTxn.markStart(time.Now())

	newTxn.IsAsync = true
	newTxn.Attrs = &attributes{}
	newTxn.Name = name
	newTxn.TraceID = oldTxn.TraceID
	newTxn.TraceIDEncoded = oldTxn.TraceIDEncoded
	newTxn.SpanID = oldTxn.SpanID
	newTxn.AsyncID = oldTxn.genNextAsyncID()
	newTxn.AsyncSequence = oldTxn.mainThread.sequenceCounter + 1

	stack := oldTxn.mainThread.stack
	if length := len(stack); length > 0 {
		stack[length-1].nextAsyncID = &newTxn.AsyncID
	} else {
		return nil
	}

	if oldTxn.finished {
		thd.txn = newTxn
		// If the transaction has finished, return the same thread.
		return newTransaction(thd)
	}

	return newTransaction(&thread{
		//thread: createThread(newTxn),
		thread: &newTxn.mainThread,
		txn:    newTxn,
	})
}

func endBasic(s *Segment) error {
	thd := s.StartTime.thread
	if nil == thd {
		return nil
	}
	txn := thd.txn
	var err error
	txn.Lock()
	if txn.finished { // async-nonblock
		err = errAlreadyEnded
	} else {
		err = endBasicSegment(&txn.txnData, thd.thread, s.StartTime.start, time.Now(), s.Name)
	}
	txn.Unlock()
	return err
}

func endDatastore(s *DatastoreSegment) error {
	thd := s.StartTime.thread
	if nil == thd {
		return nil
	}
	txn := thd.txn
	txn.Lock()
	defer txn.Unlock()

	if txn.finished {
		return errAlreadyEnded
	}
	if txn.Config.HighSecurity {
		s.QueryParameters = nil
	}
	if !txn.Config.DatastoreTracer.QueryParameters.Enabled {
		s.QueryParameters = nil
	}
	if txn.Reply.SecurityPolicies.RecordSQL.IsSet() {
		s.QueryParameters = nil
		if !txn.Reply.SecurityPolicies.RecordSQL.Enabled() {
			s.ParameterizedQuery = ""
		}
	}
	if !txn.Config.DatastoreTracer.DatabaseNameReporting.Enabled {
		s.DatabaseName = ""
	}
	if !txn.Config.DatastoreTracer.InstanceReporting.Enabled {
		s.Host = ""
		s.PortPathOrID = ""
	}
	return endDatastoreSegment(endDatastoreParams{
		TxnData:            &txn.txnData,
		Thread:             thd.thread,
		Start:              s.StartTime.start,
		Now:                time.Now(),
		Product:            string(s.Product),
		Collection:         s.Collection,
		Operation:          s.Operation,
		ParameterizedQuery: s.ParameterizedQuery,
		QueryParameters:    s.QueryParameters,
		Host:               s.Host,
		PortPathOrID:       s.PortPathOrID,
		Database:           s.DatabaseName,
		ThisHost:           txn.appRun.Config.hostname,
	})
}

func externalSegmentMethod(s *ExternalSegment) string {
	if "" != s.Procedure {
		return s.Procedure
	}
	r := s.Request
	if nil != s.Response && nil != s.Response.Request {
		r = s.Response.Request
	}

	if nil != r {
		if "" != r.Method {
			return r.Method
		}
		// Golang's http package states that when a client's Request has
		// an empty string for Method, the method is GET.
		return "GET"
	}

	return ""
}

func externalSegmentURL(s *ExternalSegment) (*url.URL, error) {
	if "" != s.URL {
		return url.Parse(s.URL)
	}
	r := s.Request
	if nil != s.Response && nil != s.Response.Request {
		r = s.Response.Request
	}
	if r != nil {
		return r.URL, nil
	}
	return nil, nil
}

func endExternal(s *ExternalSegment) error {
	thd := s.StartTime.thread
	if nil == thd {
		return nil
	}
	txn := thd.txn
	txn.Lock()
	defer txn.Unlock()

	if txn.finished {
		return errAlreadyEnded
	}
	u, err := externalSegmentURL(s)
	if nil != err {
		return err
	}
	return endExternalSegment(endExternalParams{
		NextSpanID: s.NextSpanID,
		TxnData:    &txn.txnData,
		Thread:     thd.thread,
		Start:      s.StartTime.start,
		Now:        time.Now(),
		Logger:     txn.Config.Logger,
		Response:   s.Response,
		URL:        u,
		Host:       s.Host,
		Library:    s.Library,
		Method:     externalSegmentMethod(s),
		StatusCode: s.statusCode,
	})
}

func endMessage(s *MessageProducerSegment) error {
	thd := s.StartTime.thread
	if nil == thd {
		return nil
	}
	txn := thd.txn
	txn.Lock()
	defer txn.Unlock()

	if txn.finished {
		return errAlreadyEnded
	}

	if "" == s.DestinationType {
		s.DestinationType = MessageQueue
	}

	return endMessageSegment(endMessageParams{
		TxnData:         &txn.txnData,
		Thread:          thd.thread,
		Start:           s.StartTime.start,
		Now:             time.Now(),
		Library:         s.Library,
		Logger:          txn.Config.Logger,
		DestinationName: s.DestinationName,
		DestinationType: string(s.DestinationType),
		DestinationTemp: s.DestinationTemporary,
	})
}

// oldCATOutboundHeaders generates the Old CAT and Synthetics headers, depending
// on whether Old CAT is enabled or any Synthetics functionality has been
// triggered in the agent.
func oldCATOutboundHeaders(txn *txn) http.Header {
	txn.Lock()
	defer txn.Unlock()

	if txn.finished {
		return http.Header{}
	}

	metadata, err := txn.CrossProcess.CreateCrossProcessMetadata(txn.Name, txn.Config.AppName)

	if err != nil {
		txn.Config.Logger.Debug("error generating outbound headers", map[string]interface{}{
			"error": err,
		})

		// It's possible for CreateCrossProcessMetadata() to error and still have a
		// Synthetics header, so we'll still fall through to returning headers
		// based on whatever metadata was returned.
	}

	return metadataToHTTPHeader(metadata)
}

func outboundHeaders(s *ExternalSegment) http.Header {
	thd := s.StartTime.thread
	header := http.Header{}

	if nil == thd {
		return header
	}
	// txn := thd.txn
	// hdr := oldCATOutboundHeaders(txn)

	// hdr may be empty, or it may contain headers.  If DistributedTracer
	// is enabled, add more to the existing hdr
	thd.CreateDistributedTracePayload(header, s.NextSpanID)

	return header
}

const (
	maxSampledDistributedPayloads = 35
)

func (thd *thread) CreateDistributedTracePayload(hdrs http.Header, nextSpanID int64) {
	txn := thd.txn
	txn.Lock()
	defer txn.Unlock()

	/*
		if !txn.BetterCAT.Enabled {
			return
		}

		support := &txn.DistributedTracingSupport

		excludeNRHeader := thd.Config.DistributedTracer.ExcludeNewRelicHeader
		if txn.finished {
			support.TraceContextCreateException = true
			if !excludeNRHeader {
				support.CreatePayloadException = true
			}
			return
		}

		if "" == txn.Reply.AccountID || "" == txn.Reply.TrustedAccountKey {
			// We can't create a payload:  The application is not yet
			// connected or serverless distributed tracing configuration was
			// not provided.
			return
		}

		txn.numPayloadsCreated++

		p := &payload{}

		// Calculate sampled first since this also changes the value for the
		// priority
		sampled := txn.lazilyCalculateSampled()
		if txn.shouldCreateSpanGUID() {
			p.ID = txn.CurrentSpanIdentifier(thd.thread)
		}

		p.Type = callerTypeApp
		p.Account = txn.Reply.AccountID
		p.App = txn.Reply.PrimaryAppID
		p.TracedID = txn.BetterCAT.TraceID
		p.Priority = txn.BetterCAT.Priority
		p.Timestamp.Set(txn.Reply.DistributedTraceTimestampGenerator())
		p.TrustedAccountKey = txn.Reply.TrustedAccountKey
		p.TransactionID = txn.BetterCAT.TxnID // Set the transaction ID to the transaction guid.
		if nil != txn.BetterCAT.Inbound {
			p.NonTrustedTraceState = txn.BetterCAT.Inbound.NonTrustedTraceState
			p.OriginalTraceState = txn.BetterCAT.Inbound.OriginalTraceState
		}

		// limit the number of outbound sampled=true payloads to prevent too
		// many downstream sampled events.
		p.SetSampled(false)
		if txn.numPayloadsCreated < maxSampledDistributedPayloads {
			p.SetSampled(sampled)
		}

		support.TraceContextCreateSuccess = true

		if !excludeNRHeader {
			hdrs.Set(DistributedTraceNewRelicHeader, p.NRHTTPSafe())
			support.CreatePayloadSuccess = true
		}

		// ID must be present in the Traceparent header when span events are
		// enabled, even if the transaction is not sampled.  Note that this
		// assignment occurs after setting the Newrelic header since the ID
		// field of the Newrelic header should be empty if span events are
		// disabled or the transaction is not sampled.
		if p.ID == "" {
			p.ID = txn.CurrentSpanIdentifier(thd.thread)
		}
		hdrs.Set(DistributedTraceW3CTraceParentHeader, p.W3CTraceParent())

		if !txn.Config.SpanEvents.Enabled {
			p.ID = ""
		}
		if !txn.Config.TransactionEvents.Enabled {
			p.TransactionID = ""
		}
		hdrs.Set(DistributedTraceW3CTraceStateHeader, p.W3CTraceState())
	*/
	if nextSpanID == 0 || nextSpanID == -1 {
		return
	}

	inboundMetadata := txn.CrossProcess.InboundMetadata
	pinpointTraceid := inboundMetadata.PinpointTraceid
	if pinpointTraceid == "" {
		agentID := txn.app.config.AgentID
		startTime := strconv.FormatInt(txn.app.startTime, 10)
		sequenceID := strconv.FormatInt(txn.SequenceID, 10)
		pinpointTraceid = strings.Join([]string{agentID, startTime, sequenceID}, "^")
	}
	spanID := inboundMetadata.PinpointSpanid
	if spanID == 0 || spanID == -1 {
		spanID = txn.SpanID
	}

	hdrs.Set(cat.PinpointTraceidName, pinpointTraceid)
	hdrs.Set(cat.PinpointPappnameName, txn.app.config.AppName)
	hdrs.Set(cat.PinpointPapptypeName, strconv.Itoa(int(txn.app.config.ServiceType)))
	hdrs.Set(cat.PinpointPspanidName, strconv.FormatInt(spanID, 10))
	hdrs.Set(cat.PinpointSpanidName, strconv.FormatInt(nextSpanID, 10))
	hdrs.Set(cat.PinpointFlagsName, "0")
}

var (
	errOutboundPayloadCreated   = errors.New("outbound payload already created")
	errAlreadyAccepted          = errors.New("AcceptDistributedTraceHeaders has already been called")
	errInboundPayloadDTDisabled = errors.New("DistributedTracer must be enabled to accept an inbound payload")
	errTrustedAccountKey        = errors.New("trusted account key missing or does not match")
)

func (txn *txn) AcceptDistributedTraceHeaders(t TransportType, hdrs http.Header) error {
	txn.Lock()
	defer txn.Unlock()

	return txn.acceptDistributedTraceHeadersLocked(t, hdrs)
}

func (txn *txn) acceptDistributedTraceHeadersLocked(t TransportType, hdrs http.Header) error {

	if !txn.BetterCAT.Enabled {
		return errInboundPayloadDTDisabled
	}

	if txn.finished {
		return errAlreadyEnded
	}

	support := &txn.DistributedTracingSupport

	if txn.numPayloadsCreated > 0 {
		support.AcceptPayloadCreateBeforeAccept = true
		return errOutboundPayloadCreated
	}

	if txn.BetterCAT.Inbound != nil {
		support.AcceptPayloadIgnoredMultiple = true
		return errAlreadyAccepted
	}

	if nil == hdrs {
		support.AcceptPayloadNullPayload = true
		return nil
	}

	if "" == txn.Reply.AccountID || "" == txn.Reply.TrustedAccountKey {
		// We can't accept a payload:  The application is not yet
		// connected or serverless distributed tracing configuration was
		// not provided.
		return nil
	}

	txn.BetterCAT.TransportType = t.toString()

	payload, err := acceptPayload(hdrs, txn.Reply.TrustedAccountKey, support)
	if nil != err {
		return err
	}

	if nil == payload {
		return nil
	}

	// and let's also do our trustedKey check
	receivedTrustKey := payload.TrustedAccountKey
	if "" == receivedTrustKey {
		receivedTrustKey = payload.Account
	}

	// If the trust key doesn't match but we don't have any New Relic trace info, this means
	// we just got the TraceParent header, and we still need to save that info to BetterCAT
	// farther down.
	if receivedTrustKey != txn.Reply.TrustedAccountKey && payload.HasNewRelicTraceInfo {
		support.AcceptPayloadUntrustedAccount = true
		return errTrustedAccountKey
	}

	if 0 != payload.Priority {
		txn.BetterCAT.Priority = payload.Priority
	}

	// a nul payload.Sampled means the a field wasn't provided
	if nil != payload.Sampled {
		txn.BetterCAT.Sampled = *payload.Sampled
		txn.sampledCalculated = true
	}

	txn.BetterCAT.Inbound = payload
	txn.BetterCAT.TraceID = payload.TracedID

	if tm := payload.Timestamp.Time(); txn.Start.After(tm) {
		txn.BetterCAT.Inbound.TransportDuration = txn.Start.Sub(tm)
	}

	return nil
}

func (txn *txn) Application() *Application {
	return newApplication(txn.app)
}

// Note that Agent attributes added to spans must be on the allowed list of
// span attributes, which you can find in attributes.go
func (thd *thread) AddAgentSpanAttribute(key string, val string) {
	txn := thd.txn
	txn.Lock()
	defer txn.Unlock()
	thd.thread.AddAgentSpanAttribute(key, val)
}

func (thd *thread) AddUserSpanAttribute(key string, val interface{}) error {
	txn := thd.txn
	txn.Lock()
	defer txn.Unlock()

	if outputDests := applyAttributeConfig(thd.Attrs.config, key, destSpan); 0 == outputDests {
		return nil
	}

	if txn.Config.HighSecurity {
		return errHighSecurityEnabled
	}

	if !txn.Reply.SecurityPolicies.CustomParameters.Enabled() {
		return errSecurityPolicy
	}

	thd.thread.AddUserSpanAttribute(key, val)
	return nil
}

var (
	// Ensure that txn implements AddAgentAttributer to avoid breaking
	// integration package type assertions.
	_ internal.AddAgentAttributer = &txn{}
)

func (txn *txn) AddAgentAttribute(name string, stringVal string, otherVal interface{}) {
	txn.Lock()
	defer txn.Unlock()

	if txn.finished {
		return
	}
	txn.Attrs.Agent.Add(name, stringVal, otherVal)
}

func (thd *thread) GetTraceMetadata() (metadata TraceMetadata) {
	txn := thd.txn
	txn.Lock()
	defer txn.Unlock()

	if txn.finished {
		return
	}

	if txn.BetterCAT.Enabled {
		metadata.TraceID = txn.BetterCAT.TraceID
		if txn.shouldCollectSpanEvents() {
			metadata.SpanID = txn.CurrentSpanIdentifier(thd.thread)
		}
	}

	return
}

func (thd *thread) GetLinkingMetadata() (metadata LinkingMetadata) {
	txn := thd.txn
	metadata.EntityName = txn.appRun.firstAppName
	metadata.EntityType = "SERVICE"
	metadata.EntityGUID = txn.appRun.Reply.EntityGUID
	metadata.Hostname = txn.appRun.Config.hostname

	md := thd.GetTraceMetadata()
	metadata.TraceID = md.TraceID
	metadata.SpanID = md.SpanID

	return
}

func (txn *txn) IsSampled() bool {
	txn.Lock()
	defer txn.Unlock()

	if txn.finished {
		return false
	}

	return txn.lazilyCalculateSampled()
}
