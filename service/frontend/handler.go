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

package frontend

import (
	"encoding/json"
	"log"
	"sync"

	"github.com/pborman/uuid"
	"github.com/uber/cadence/.gen/go/cadence"
	h "github.com/uber/cadence/.gen/go/history"
	m "github.com/uber/cadence/.gen/go/matching"
	gen "github.com/uber/cadence/.gen/go/shared"
	"github.com/uber/cadence/client/history"
	"github.com/uber/cadence/client/matching"
	"github.com/uber/cadence/common"
	"github.com/uber/cadence/common/cache"
	"github.com/uber/cadence/common/persistence"
	"github.com/uber/cadence/common/service"

	"github.com/uber-common/bark"
	"github.com/uber/tchannel-go/thrift"
)

var _ cadence.TChanWorkflowService = (*WorkflowHandler)(nil)

type (
	// WorkflowHandler - Thrift handler inteface for workflow service
	WorkflowHandler struct {
		domainCache        cache.DomainCache
		metadataMgr        persistence.MetadataManager
		historyMgr         persistence.HistoryManager
		visibitiltyMgr     persistence.VisibilityManager
		history            history.Client
		matching           matching.Client
		tokenSerializer    common.TaskTokenSerializer
		hSerializerFactory persistence.HistorySerializerFactory
		startWG            sync.WaitGroup
		service.Service
	}

	getHistoryContinuationToken struct {
		runID            string
		nextEventID      int64
		persistenceToken []byte
	}
)

const (
	defaultVisibilityMaxPageSize = 1000
	defaultHistoryMaxPageSize    = 1000
)

var (
	errDomainNotSet         = &gen.BadRequestError{Message: "Domain not set on request."}
	errTaskTokenNotSet      = &gen.BadRequestError{Message: "Task token not set on request."}
	errTaskListNotSet       = &gen.BadRequestError{Message: "TaskList is not set on request."}
	errExecutionNotSet      = &gen.BadRequestError{Message: "Execution is not set on request."}
	errWorkflowIDNotSet     = &gen.BadRequestError{Message: "WorkflowId is not set on request."}
	errRunIDNotSet          = &gen.BadRequestError{Message: "RunId is not set on request."}
	errInvalidRunID         = &gen.BadRequestError{Message: "Invalid RunId."}
	errInvalidNextPageToken = &gen.BadRequestError{Message: "Invalid NextPageToken."}
)

// NewWorkflowHandler creates a thrift handler for the cadence service
func NewWorkflowHandler(
	sVice service.Service, metadataMgr persistence.MetadataManager,
	historyMgr persistence.HistoryManager, visibilityMgr persistence.VisibilityManager) (*WorkflowHandler, []thrift.TChanServer) {
	handler := &WorkflowHandler{
		Service:            sVice,
		metadataMgr:        metadataMgr,
		historyMgr:         historyMgr,
		visibitiltyMgr:     visibilityMgr,
		tokenSerializer:    common.NewJSONTaskTokenSerializer(),
		hSerializerFactory: persistence.NewHistorySerializerFactory(),
		domainCache:        cache.NewDomainCache(metadataMgr, sVice.GetLogger()),
	}
	// prevent us from trying to serve requests before handler's Start() is complete
	handler.startWG.Add(1)
	return handler, []thrift.TChanServer{cadence.NewTChanWorkflowServiceServer(handler)}
}

// Start starts the handler
func (wh *WorkflowHandler) Start(thriftService []thrift.TChanServer) error {
	wh.Service.Start(thriftService)
	var err error
	wh.history, err = wh.Service.GetClientFactory().NewHistoryClient()
	if err != nil {
		return err
	}
	wh.matching, err = wh.Service.GetClientFactory().NewMatchingClient()
	if err != nil {
		return err
	}
	wh.startWG.Done()
	return nil
}

// Stop stops the handler
func (wh *WorkflowHandler) Stop() {
	wh.Service.Stop()
}

// IsHealthy - Health endpoint.
func (wh *WorkflowHandler) IsHealthy(ctx thrift.Context) (bool, error) {
	log.Println("Workflow Health endpoint reached.")
	return true, nil
}

// RegisterDomain creates a new domain which can be used as a container for all resources.  Domain is a top level
// entity within Cadence, used as a container for all resources like workflow executions, tasklists, etc.  Domain
// acts as a sandbox and provides isolation for all resources within the domain.  All resources belongs to exactly one
// domain.
func (wh *WorkflowHandler) RegisterDomain(ctx thrift.Context, registerRequest *gen.RegisterDomainRequest) error {
	wh.startWG.Wait()

	if !registerRequest.IsSetName() || registerRequest.GetName() == "" {
		return errDomainNotSet
	}

	response, err := wh.metadataMgr.CreateDomain(&persistence.CreateDomainRequest{
		Name:        registerRequest.GetName(),
		Status:      persistence.DomainStatusRegistered,
		OwnerEmail:  registerRequest.GetOwnerEmail(),
		Description: registerRequest.GetDescription(),
		Retention:   registerRequest.GetWorkflowExecutionRetentionPeriodInDays(),
		EmitMetric:  registerRequest.GetEmitMetric(),
	})

	if err != nil {
		return wrapError(err)
	}

	// TODO: Log through logging framework.  We need to have good auditing of domain CRUD
	wh.GetLogger().Debugf("Register domain succeeded for name: %v, Id: %v", registerRequest.GetName(), response.ID)
	return nil
}

// DescribeDomain returns the information and configuration for a registered domain.
func (wh *WorkflowHandler) DescribeDomain(ctx thrift.Context,
	describeRequest *gen.DescribeDomainRequest) (*gen.DescribeDomainResponse, error) {
	wh.startWG.Wait()

	if !describeRequest.IsSetName() {
		return nil, errDomainNotSet
	}

	resp, err := wh.metadataMgr.GetDomain(&persistence.GetDomainRequest{
		Name: describeRequest.GetName(),
	})

	if err != nil {
		return nil, wrapError(err)
	}

	response := gen.NewDescribeDomainResponse()
	response.DomainInfo, response.Configuration = createDomainResponse(resp.Info, resp.Config)

	return response, nil
}

// UpdateDomain is used to update the information and configuration for a registered domain.
func (wh *WorkflowHandler) UpdateDomain(ctx thrift.Context,
	updateRequest *gen.UpdateDomainRequest) (*gen.UpdateDomainResponse, error) {
	wh.startWG.Wait()

	if !updateRequest.IsSetName() {
		return nil, errDomainNotSet
	}

	domainName := updateRequest.GetName()

	getResponse, err0 := wh.metadataMgr.GetDomain(&persistence.GetDomainRequest{
		Name: domainName,
	})

	if err0 != nil {
		return nil, wrapError(err0)
	}

	info := getResponse.Info
	config := getResponse.Config

	if updateRequest.IsSetUpdatedInfo() {
		updatedInfo := updateRequest.GetUpdatedInfo()
		if updatedInfo.IsSetDescription() {
			info.Description = updatedInfo.GetDescription()
		}
		if updatedInfo.IsSetOwnerEmail() {
			info.OwnerEmail = updatedInfo.GetOwnerEmail()
		}
	}

	if updateRequest.IsSetConfiguration() {
		updatedConfig := updateRequest.GetConfiguration()
		if updatedConfig.IsSetEmitMetric() {
			config.EmitMetric = updatedConfig.GetEmitMetric()
		}
		if updatedConfig.IsSetWorkflowExecutionRetentionPeriodInDays() {
			config.Retention = updatedConfig.GetWorkflowExecutionRetentionPeriodInDays()
		}
	}

	err := wh.metadataMgr.UpdateDomain(&persistence.UpdateDomainRequest{
		Info:   info,
		Config: config,
	})
	if err != nil {
		return nil, wrapError(err)
	}

	response := gen.NewUpdateDomainResponse()
	response.DomainInfo, response.Configuration = createDomainResponse(info, config)
	return response, nil
}

// DeprecateDomain us used to update status of a registered domain to DEPRECATED.  Once the domain is deprecated
// it cannot be used to start new workflow executions.  Existing workflow executions will continue to run on
// deprecated domains.
func (wh *WorkflowHandler) DeprecateDomain(ctx thrift.Context, deprecateRequest *gen.DeprecateDomainRequest) error {
	wh.startWG.Wait()

	if !deprecateRequest.IsSetName() {
		return errDomainNotSet
	}

	domainName := deprecateRequest.GetName()

	getResponse, err0 := wh.metadataMgr.GetDomain(&persistence.GetDomainRequest{
		Name: domainName,
	})

	if err0 != nil {
		return wrapError(err0)
	}

	info := getResponse.Info
	info.Status = persistence.DomainStatusDeprecated
	config := getResponse.Config

	return wh.metadataMgr.UpdateDomain(&persistence.UpdateDomainRequest{
		Info:   info,
		Config: config,
	})
}

// PollForActivityTask - Poll for an activity task.
func (wh *WorkflowHandler) PollForActivityTask(
	ctx thrift.Context,
	pollRequest *gen.PollForActivityTaskRequest) (*gen.PollForActivityTaskResponse, error) {
	wh.startWG.Wait()

	wh.Service.GetLogger().Debug("Received PollForActivityTask")
	if !pollRequest.IsSetDomain() {
		return nil, errDomainNotSet
	}

	if !pollRequest.IsSetTaskList() || !pollRequest.GetTaskList().IsSetName() || pollRequest.GetTaskList().GetName() == "" {
		return nil, errTaskListNotSet
	}

	domainName := pollRequest.GetDomain()
	info, _, err := wh.domainCache.GetDomain(domainName)
	if err != nil {
		return nil, wrapError(err)
	}

	resp, err := wh.matching.PollForActivityTask(ctx, &m.PollForActivityTaskRequest{
		DomainUUID:  common.StringPtr(info.ID),
		PollRequest: pollRequest,
	})
	if err != nil {
		wh.Service.GetLogger().Errorf(
			"PollForActivityTask failed. TaskList: %v, Error: %v", pollRequest.GetTaskList().GetName(), err)
	}
	return resp, wrapError(err)
}

// PollForDecisionTask - Poll for a decision task.
func (wh *WorkflowHandler) PollForDecisionTask(
	ctx thrift.Context,
	pollRequest *gen.PollForDecisionTaskRequest) (*gen.PollForDecisionTaskResponse, error) {
	wh.startWG.Wait()

	wh.Service.GetLogger().Debug("Received PollForDecisionTask")
	if !pollRequest.IsSetDomain() {
		return nil, errDomainNotSet
	}

	if !pollRequest.IsSetTaskList() || !pollRequest.GetTaskList().IsSetName() || pollRequest.GetTaskList().GetName() == "" {
		return nil, errTaskListNotSet
	}

	domainName := pollRequest.GetDomain()
	info, _, err := wh.domainCache.GetDomain(domainName)
	if err != nil {
		return nil, wrapError(err)
	}

	wh.Service.GetLogger().Infof("Poll for decision domain name: %v", domainName)
	wh.Service.GetLogger().Infof("Poll for decision request domainID: %v", info.ID)

	matchingResp, err := wh.matching.PollForDecisionTask(ctx, &m.PollForDecisionTaskRequest{
		DomainUUID:  common.StringPtr(info.ID),
		PollRequest: pollRequest,
	})
	if err != nil {
		wh.Service.GetLogger().Errorf(
			"PollForDecisionTask failed. TaskList: %v, Error: %v", pollRequest.GetTaskList().GetName(), err)
		return nil, wrapError(err)
	}

	var history *gen.History
	var persistenceToken []byte
	var continuation []byte
	if matchingResp.IsSetWorkflowExecution() {
		// Non-empty response. Get the history
		history, persistenceToken, err = wh.getHistory(
			info.ID, *matchingResp.GetWorkflowExecution(), matchingResp.GetStartedEventId()+1, defaultHistoryMaxPageSize, nil)
		if err != nil {
			return nil, wrapError(err)
		}

		continuation, err =
			getSerializedGetHistoryToken(persistenceToken, matchingResp.GetWorkflowExecution().GetRunId(), history, matchingResp.GetStartedEventId()+1)
		if err != nil {
			return nil, wrapError(err)
		}
	}

	return createPollForDecisionTaskResponse(matchingResp, history, continuation), nil
}

// RecordActivityTaskHeartbeat - Record Activity Task Heart beat.
func (wh *WorkflowHandler) RecordActivityTaskHeartbeat(
	ctx thrift.Context,
	heartbeatRequest *gen.RecordActivityTaskHeartbeatRequest) (*gen.RecordActivityTaskHeartbeatResponse, error) {
	wh.startWG.Wait()

	wh.Service.GetLogger().Debug("Received RecordActivityTaskHeartbeat")
	if !heartbeatRequest.IsSetTaskToken() {
		return nil, errTaskTokenNotSet
	}
	taskToken, err := wh.tokenSerializer.Deserialize(heartbeatRequest.GetTaskToken())
	if err != nil {
		return nil, wrapError(err)
	}
	if taskToken.DomainID == "" {
		return nil, errDomainNotSet
	}

	resp, err := wh.history.RecordActivityTaskHeartbeat(ctx, &h.RecordActivityTaskHeartbeatRequest{
		DomainUUID:       common.StringPtr(taskToken.DomainID),
		HeartbeatRequest: heartbeatRequest,
	})
	return resp, wrapError(err)
}

// RespondActivityTaskCompleted - response to an activity task
func (wh *WorkflowHandler) RespondActivityTaskCompleted(
	ctx thrift.Context,
	completeRequest *gen.RespondActivityTaskCompletedRequest) error {
	wh.startWG.Wait()

	if !completeRequest.IsSetTaskToken() {
		return errTaskTokenNotSet
	}
	taskToken, err := wh.tokenSerializer.Deserialize(completeRequest.GetTaskToken())
	if err != nil {
		return wrapError(err)
	}
	if taskToken.DomainID == "" {
		return errDomainNotSet
	}

	err = wh.history.RespondActivityTaskCompleted(ctx, &h.RespondActivityTaskCompletedRequest{
		DomainUUID:      common.StringPtr(taskToken.DomainID),
		CompleteRequest: completeRequest,
	})
	if err != nil {
		logger := wh.getLoggerForTask(completeRequest.GetTaskToken())
		logger.Errorf("RespondActivityTaskCompleted. Error: %v", err)
	}
	return wrapError(err)
}

// RespondActivityTaskFailed - response to an activity task failure
func (wh *WorkflowHandler) RespondActivityTaskFailed(
	ctx thrift.Context,
	failedRequest *gen.RespondActivityTaskFailedRequest) error {
	wh.startWG.Wait()

	if !failedRequest.IsSetTaskToken() {
		return errTaskTokenNotSet
	}
	taskToken, err := wh.tokenSerializer.Deserialize(failedRequest.GetTaskToken())
	if err != nil {
		return wrapError(err)
	}
	if taskToken.DomainID == "" {
		return errDomainNotSet
	}

	err = wh.history.RespondActivityTaskFailed(ctx, &h.RespondActivityTaskFailedRequest{
		DomainUUID:    common.StringPtr(taskToken.DomainID),
		FailedRequest: failedRequest,
	})
	if err != nil {
		logger := wh.getLoggerForTask(failedRequest.GetTaskToken())
		logger.Errorf("RespondActivityTaskFailed. Error: %v", err)
	}
	return wrapError(err)

}

// RespondActivityTaskCanceled - called to cancel an activity task
func (wh *WorkflowHandler) RespondActivityTaskCanceled(
	ctx thrift.Context,
	cancelRequest *gen.RespondActivityTaskCanceledRequest) error {
	wh.startWG.Wait()

	if !cancelRequest.IsSetTaskToken() {
		return errTaskTokenNotSet
	}
	taskToken, err := wh.tokenSerializer.Deserialize(cancelRequest.GetTaskToken())
	if err != nil {
		return wrapError(err)
	}
	if taskToken.DomainID == "" {
		return errDomainNotSet
	}

	err = wh.history.RespondActivityTaskCanceled(ctx, &h.RespondActivityTaskCanceledRequest{
		DomainUUID:    common.StringPtr(taskToken.DomainID),
		CancelRequest: cancelRequest,
	})
	if err != nil {
		logger := wh.getLoggerForTask(cancelRequest.GetTaskToken())
		logger.Errorf("RespondActivityTaskCanceled. Error: %v", err)
	}
	return wrapError(err)

}

// RespondDecisionTaskCompleted - response to a decision task
func (wh *WorkflowHandler) RespondDecisionTaskCompleted(
	ctx thrift.Context,
	completeRequest *gen.RespondDecisionTaskCompletedRequest) error {
	wh.startWG.Wait()

	if !completeRequest.IsSetTaskToken() {
		return errTaskTokenNotSet
	}
	taskToken, err := wh.tokenSerializer.Deserialize(completeRequest.GetTaskToken())
	if err != nil {
		return wrapError(err)
	}
	if taskToken.DomainID == "" {
		return errDomainNotSet
	}

	err = wh.history.RespondDecisionTaskCompleted(ctx, &h.RespondDecisionTaskCompletedRequest{
		DomainUUID:      common.StringPtr(taskToken.DomainID),
		CompleteRequest: completeRequest,
	})
	if err != nil {
		logger := wh.getLoggerForTask(completeRequest.GetTaskToken())
		logger.Errorf("RespondDecisionTaskCompleted. Error: %v", err)
	}
	return wrapError(err)
}

// StartWorkflowExecution - Creates a new workflow execution
func (wh *WorkflowHandler) StartWorkflowExecution(
	ctx thrift.Context,
	startRequest *gen.StartWorkflowExecutionRequest) (*gen.StartWorkflowExecutionResponse, error) {
	wh.startWG.Wait()

	wh.Service.GetLogger().Debugf("Received StartWorkflowExecution. WorkflowID: %v", startRequest.GetWorkflowId())

	if !startRequest.IsSetDomain() {
		return nil, errDomainNotSet
	}

	if !startRequest.IsSetWorkflowId() || startRequest.GetWorkflowId() == "" {
		return nil, &gen.BadRequestError{Message: "WorkflowId is not set on request."}
	}

	if !startRequest.IsSetWorkflowType() || !startRequest.GetWorkflowType().IsSetName() || startRequest.GetWorkflowType().GetName() == "" {
		return nil, &gen.BadRequestError{Message: "WorkflowType is not set on request."}
	}

	if !startRequest.IsSetTaskList() || !startRequest.GetTaskList().IsSetName() || startRequest.GetTaskList().GetName() == "" {
		return nil, errTaskListNotSet
	}

	if !startRequest.IsSetExecutionStartToCloseTimeoutSeconds() || startRequest.GetExecutionStartToCloseTimeoutSeconds() <= 0 {
		return nil, &gen.BadRequestError{Message: "A valid ExecutionStartToCloseTimeoutSeconds is not set on request."}
	}

	if !startRequest.IsSetTaskStartToCloseTimeoutSeconds() || startRequest.GetExecutionStartToCloseTimeoutSeconds() <= 0 {
		return nil, &gen.BadRequestError{Message: "A valid TaskStartToCloseTimeoutSeconds is not set on request."}
	}

	domainName := startRequest.GetDomain()
	wh.Service.GetLogger().Infof("Start workflow execution request domain: %v", domainName)
	info, _, err := wh.domainCache.GetDomain(domainName)
	if err != nil {
		return nil, wrapError(err)
	}

	wh.Service.GetLogger().Infof("Start workflow execution request domainID: %v", info.ID)

	resp, err := wh.history.StartWorkflowExecution(ctx, &h.StartWorkflowExecutionRequest{
		DomainUUID:   common.StringPtr(info.ID),
		StartRequest: startRequest,
	})
	if err != nil {
		wh.Service.GetLogger().Errorf("StartWorkflowExecution failed. WorkflowID: %v. Error: %v", startRequest.GetWorkflowId(), err)
	}
	return resp, wrapError(err)
}

// GetWorkflowExecutionHistory - retrieves the hisotry of workflow execution
func (wh *WorkflowHandler) GetWorkflowExecutionHistory(
	ctx thrift.Context,
	getRequest *gen.GetWorkflowExecutionHistoryRequest) (*gen.GetWorkflowExecutionHistoryResponse, error) {
	wh.startWG.Wait()

	if !getRequest.IsSetDomain() {
		return nil, errDomainNotSet
	}

	if !getRequest.IsSetExecution() {
		return nil, errExecutionNotSet
	}

	if !getRequest.GetExecution().IsSetWorkflowId() {
		return nil, errWorkflowIDNotSet
	}

	if !getRequest.GetExecution().IsSetRunId() {
		return nil, errRunIDNotSet
	}

	if uuid.Parse(getRequest.GetExecution().GetRunId()) == nil {
		return nil, errInvalidRunID
	}

	if !getRequest.IsSetMaximumPageSize() || getRequest.GetMaximumPageSize() == 0 {
		getRequest.MaximumPageSize = common.Int32Ptr(defaultHistoryMaxPageSize)
	}

	domainName := getRequest.GetDomain()
	info, _, err := wh.domainCache.GetDomain(domainName)
	if err != nil {
		return nil, wrapError(err)
	}

	token := &getHistoryContinuationToken{}
	if getRequest.IsSetNextPageToken() {
		token, err = deserializeGetHistoryToken(getRequest.GetNextPageToken())
		if err != nil {
			return nil, errInvalidNextPageToken
		}
	} else {
		response, err := wh.history.GetWorkflowExecutionNextEventID(ctx, &h.GetWorkflowExecutionNextEventIDRequest{
			DomainUUID: common.StringPtr(info.ID),
			Execution:  getRequest.GetExecution(),
		})
		if err != nil {
			return nil, wrapError(err)
		}
		token.nextEventID = response.GetEventId()
		token.runID = response.GetRunId()
	}

	we := gen.WorkflowExecution{
		WorkflowId: getRequest.GetExecution().WorkflowId,
		RunId:      common.StringPtr(token.runID),
	}
	history, persistenceToken, err :=
		wh.getHistory(info.ID, we, token.nextEventID, getRequest.GetMaximumPageSize(), getRequest.GetNextPageToken())
	if err != nil {
		return nil, wrapError(err)
	}

	nextToken, err := getSerializedGetHistoryToken(persistenceToken, token.runID, history, token.nextEventID)
	if err != nil {
		return nil, wrapError(err)
	}

	return createGetWorkflowExecutionHistoryResponse(history, token.nextEventID, nextToken), nil
}

// SignalWorkflowExecution is used to send a signal event to running workflow execution.  This results in
// WorkflowExecutionSignaled event recorded in the history and a decision task being created for the execution.
func (wh *WorkflowHandler) SignalWorkflowExecution(ctx thrift.Context,
	signalRequest *gen.SignalWorkflowExecutionRequest) error {
	wh.startWG.Wait()

	if !signalRequest.IsSetDomain() {
		return errDomainNotSet
	}

	if !signalRequest.IsSetWorkflowExecution() {
		return errExecutionNotSet
	}

	if !signalRequest.GetWorkflowExecution().IsSetWorkflowId() {
		return errWorkflowIDNotSet
	}

	if signalRequest.GetWorkflowExecution().IsSetRunId() &&
		uuid.Parse(signalRequest.GetWorkflowExecution().GetRunId()) == nil {
		return errInvalidRunID
	}

	if !signalRequest.IsSetSignalName() {
		return &gen.BadRequestError{Message: "SignalName is not set on request."}
	}

	domainName := signalRequest.GetDomain()
	info, _, err := wh.domainCache.GetDomain(domainName)
	if err != nil {
		return wrapError(err)
	}

	err = wh.history.SignalWorkflowExecution(ctx, &h.SignalWorkflowExecutionRequest{
		DomainUUID:    common.StringPtr(info.ID),
		SignalRequest: signalRequest,
	})

	return wrapError(err)
}

// TerminateWorkflowExecution terminates an existing workflow execution by recording WorkflowExecutionTerminated event
// in the history and immediately terminating the execution instance.
func (wh *WorkflowHandler) TerminateWorkflowExecution(ctx thrift.Context,
	terminateRequest *gen.TerminateWorkflowExecutionRequest) error {
	wh.startWG.Wait()

	if !terminateRequest.IsSetDomain() {
		return errDomainNotSet
	}

	if !terminateRequest.IsSetWorkflowExecution() {
		return errExecutionNotSet
	}

	if !terminateRequest.GetWorkflowExecution().IsSetWorkflowId() {
		return errWorkflowIDNotSet
	}

	if terminateRequest.GetWorkflowExecution().IsSetRunId() &&
		uuid.Parse(terminateRequest.GetWorkflowExecution().GetRunId()) == nil {
		return errInvalidRunID
	}

	domainName := terminateRequest.GetDomain()
	info, _, err := wh.domainCache.GetDomain(domainName)
	if err != nil {
		return wrapError(err)
	}

	err = wh.history.TerminateWorkflowExecution(ctx, &h.TerminateWorkflowExecutionRequest{
		DomainUUID:       common.StringPtr(info.ID),
		TerminateRequest: terminateRequest,
	})

	return wrapError(err)
}

// RequestCancelWorkflowExecution - requests to cancel a workflow execution
func (wh *WorkflowHandler) RequestCancelWorkflowExecution(
	ctx thrift.Context,
	cancelRequest *gen.RequestCancelWorkflowExecutionRequest) error {
	wh.startWG.Wait()

	if !cancelRequest.IsSetDomain() {
		return errDomainNotSet
	}

	if !cancelRequest.IsSetWorkflowExecution() {
		return errExecutionNotSet
	}

	if !cancelRequest.GetWorkflowExecution().IsSetWorkflowId() {
		return errWorkflowIDNotSet
	}

	if !cancelRequest.GetWorkflowExecution().IsSetRunId() {
		return errRunIDNotSet
	}

	if uuid.Parse(cancelRequest.GetWorkflowExecution().GetRunId()) == nil {
		return errInvalidRunID
	}

	domainName := cancelRequest.GetDomain()
	info, _, err := wh.domainCache.GetDomain(domainName)
	if err != nil {
		return wrapError(err)
	}

	err = wh.history.RequestCancelWorkflowExecution(ctx, &h.RequestCancelWorkflowExecutionRequest{
		DomainUUID:    common.StringPtr(info.ID),
		CancelRequest: cancelRequest,
	})

	return wrapError(err)
}

// ListOpenWorkflowExecutions - retrieves info for open workflow executions in a domain
func (wh *WorkflowHandler) ListOpenWorkflowExecutions(ctx thrift.Context,
	listRequest *gen.ListOpenWorkflowExecutionsRequest) (*gen.ListOpenWorkflowExecutionsResponse, error) {

	if !listRequest.IsSetDomain() {
		return nil, errDomainNotSet
	}

	if !listRequest.IsSetStartTimeFilter() {
		return nil, &gen.BadRequestError{
			Message: "StartTimeFilter is required",
		}
	}

	if !listRequest.GetStartTimeFilter().IsSetEarliestTime() {
		return nil, &gen.BadRequestError{
			Message: "EarliestTime in StartTimeFilter is required",
		}
	}

	if !listRequest.GetStartTimeFilter().IsSetLatestTime() {
		return nil, &gen.BadRequestError{
			Message: "LatestTime in StartTimeFilter is required",
		}
	}

	if listRequest.IsSetExecutionFilter() && listRequest.IsSetTypeFilter() {
		return nil, &gen.BadRequestError{
			Message: "Only one of ExecutionFilter or TypeFilter is allowed",
		}
	}

	if !listRequest.IsSetMaximumPageSize() || listRequest.GetMaximumPageSize() == 0 {
		listRequest.MaximumPageSize = common.Int32Ptr(defaultVisibilityMaxPageSize)
	}

	domainName := listRequest.GetDomain()
	domainInfo, _, err := wh.domainCache.GetDomain(domainName)
	if err != nil {
		return nil, wrapError(err)
	}

	baseReq := persistence.ListWorkflowExecutionsRequest{
		DomainUUID:        domainInfo.ID,
		PageSize:          int(listRequest.GetMaximumPageSize()),
		NextPageToken:     listRequest.GetNextPageToken(),
		EarliestStartTime: listRequest.GetStartTimeFilter().GetEarliestTime(),
		LatestStartTime:   listRequest.GetStartTimeFilter().GetLatestTime(),
	}

	var persistenceResp *persistence.ListWorkflowExecutionsResponse
	if listRequest.IsSetExecutionFilter() {
		persistenceResp, err = wh.visibitiltyMgr.ListOpenWorkflowExecutionsByWorkflowID(
			&persistence.ListWorkflowExecutionsByWorkflowIDRequest{
				ListWorkflowExecutionsRequest: baseReq,
				WorkflowID:                    listRequest.ExecutionFilter.GetWorkflowId(),
			})
	} else if listRequest.IsSetTypeFilter() {
		persistenceResp, err = wh.visibitiltyMgr.ListOpenWorkflowExecutionsByType(&persistence.ListWorkflowExecutionsByTypeRequest{
			ListWorkflowExecutionsRequest: baseReq,
			WorkflowTypeName:              listRequest.TypeFilter.GetName(),
		})
	} else {
		persistenceResp, err = wh.visibitiltyMgr.ListOpenWorkflowExecutions(&baseReq)
	}

	if err != nil {
		return nil, wrapError(err)
	}

	resp := gen.NewListOpenWorkflowExecutionsResponse()
	resp.Executions = persistenceResp.Executions
	resp.NextPageToken = persistenceResp.NextPageToken
	return resp, nil
}

// ListClosedWorkflowExecutions - retrieves info for closed workflow executions in a domain
func (wh *WorkflowHandler) ListClosedWorkflowExecutions(ctx thrift.Context,
	listRequest *gen.ListClosedWorkflowExecutionsRequest) (*gen.ListClosedWorkflowExecutionsResponse, error) {
	if !listRequest.IsSetDomain() {
		return nil, errDomainNotSet
	}

	if !listRequest.IsSetStartTimeFilter() {
		return nil, &gen.BadRequestError{
			Message: "StartTimeFilter is required",
		}
	}

	if !listRequest.GetStartTimeFilter().IsSetEarliestTime() {
		return nil, &gen.BadRequestError{
			Message: "EarliestTime in StartTimeFilter is required",
		}
	}

	if !listRequest.GetStartTimeFilter().IsSetLatestTime() {
		return nil, &gen.BadRequestError{
			Message: "LatestTime in StartTimeFilter is required",
		}
	}

	filterCount := 0
	if listRequest.IsSetExecutionFilter() {
		filterCount++
	}
	if listRequest.IsSetTypeFilter() {
		filterCount++
	}
	if listRequest.IsSetStatusFilter() {
		filterCount++
	}

	if filterCount > 1 {
		return nil, &gen.BadRequestError{
			Message: "Only one of ExecutionFilter, TypeFilter or StatusFilter is allowed",
		}
	}

	if !listRequest.IsSetMaximumPageSize() || listRequest.GetMaximumPageSize() == 0 {
		listRequest.MaximumPageSize = common.Int32Ptr(defaultVisibilityMaxPageSize)
	}

	domainName := listRequest.GetDomain()
	domainInfo, _, err := wh.domainCache.GetDomain(domainName)
	if err != nil {
		return nil, wrapError(err)
	}

	baseReq := persistence.ListWorkflowExecutionsRequest{
		DomainUUID:        domainInfo.ID,
		PageSize:          int(listRequest.GetMaximumPageSize()),
		NextPageToken:     listRequest.GetNextPageToken(),
		EarliestStartTime: listRequest.GetStartTimeFilter().GetEarliestTime(),
		LatestStartTime:   listRequest.GetStartTimeFilter().GetLatestTime(),
	}

	var persistenceResp *persistence.ListWorkflowExecutionsResponse
	if listRequest.IsSetExecutionFilter() {
		persistenceResp, err = wh.visibitiltyMgr.ListClosedWorkflowExecutionsByWorkflowID(
			&persistence.ListWorkflowExecutionsByWorkflowIDRequest{
				ListWorkflowExecutionsRequest: baseReq,
				WorkflowID:                    listRequest.ExecutionFilter.GetWorkflowId(),
			})
	} else if listRequest.IsSetTypeFilter() {
		persistenceResp, err = wh.visibitiltyMgr.ListClosedWorkflowExecutionsByType(&persistence.ListWorkflowExecutionsByTypeRequest{
			ListWorkflowExecutionsRequest: baseReq,
			WorkflowTypeName:              listRequest.TypeFilter.GetName(),
		})
	} else if listRequest.IsSetStatusFilter() {
		persistenceResp, err = wh.visibitiltyMgr.ListClosedWorkflowExecutionsByStatus(&persistence.ListClosedWorkflowExecutionsByStatusRequest{
			ListWorkflowExecutionsRequest: baseReq,
			Status: listRequest.GetStatusFilter(),
		})
	} else {
		persistenceResp, err = wh.visibitiltyMgr.ListClosedWorkflowExecutions(&baseReq)
	}

	if err != nil {
		return nil, wrapError(err)
	}

	resp := gen.NewListClosedWorkflowExecutionsResponse()
	resp.Executions = persistenceResp.Executions
	resp.NextPageToken = persistenceResp.NextPageToken
	return resp, nil
}

func (wh *WorkflowHandler) getHistory(domainID string, execution gen.WorkflowExecution,
	nextEventID int64, pageSize int32, nextPageToken []byte) (*gen.History, []byte, error) {

	if nextPageToken == nil {
		nextPageToken = []byte{}
	}
	historyEvents := []*gen.HistoryEvent{}

	response, err := wh.historyMgr.GetWorkflowExecutionHistory(&persistence.GetWorkflowExecutionHistoryRequest{
		DomainID:      domainID,
		Execution:     execution,
		NextEventID:   nextEventID,
		PageSize:      int(pageSize),
		NextPageToken: nextPageToken,
	})

	if err != nil {
		return nil, nil, err
	}

	for _, e := range response.Events {
		setSerializedHistoryDefaults(&e)
		s, _ := wh.hSerializerFactory.Get(e.EncodingType)
		history, err1 := s.Deserialize(&e)
		if err1 != nil {
			return nil, nil, err1
		}
		historyEvents = append(historyEvents, history.Events...)
	}

	nextPageToken = response.NextPageToken

	executionHistory := gen.NewHistory()
	executionHistory.Events = historyEvents
	return executionHistory, nextPageToken, nil
}

// sets the version and encoding types to defaults if they
// are missing from persistence. This is purely for backwards
// compatibility
func setSerializedHistoryDefaults(history *persistence.SerializedHistoryEventBatch) {
	if history.Version == 0 {
		history.Version = persistence.GetDefaultHistoryVersion()
	}
	if len(history.EncodingType) == 0 {
		history.EncodingType = persistence.DefaultEncodingType
	}
}

func (wh *WorkflowHandler) getLoggerForTask(taskToken []byte) bark.Logger {
	logger := wh.Service.GetLogger()
	task, err := wh.tokenSerializer.Deserialize(taskToken)
	if err == nil {
		logger = logger.WithFields(bark.Fields{
			"WorkflowID": task.WorkflowID,
			"RunID":      task.RunID,
			"ScheduleID": task.ScheduleID,
		})
	}
	return logger
}

func wrapError(err error) error {
	if err != nil && shouldWrapInInternalServiceError(err) {
		return &gen.InternalServiceError{Message: err.Error()}
	}
	return err
}

func shouldWrapInInternalServiceError(err error) bool {
	switch err.(type) {
	case *gen.InternalServiceError:
		return false
	case *gen.BadRequestError:
		return false
	case *gen.EntityNotExistsError:
		return false
	case *gen.WorkflowExecutionAlreadyStartedError:
		return false
	case *gen.DomainAlreadyExistsError:
		return false
	}

	return true
}

func getDomainStatus(info *persistence.DomainInfo) *gen.DomainStatus {
	switch info.Status {
	case persistence.DomainStatusRegistered:
		return gen.DomainStatusPtr(gen.DomainStatus_REGISTERED)
	case persistence.DomainStatusDeprecated:
		return gen.DomainStatusPtr(gen.DomainStatus_DEPRECATED)
	case persistence.DomainStatusDeleted:
		return gen.DomainStatusPtr(gen.DomainStatus_DELETED)
	}

	return nil
}

func createDomainResponse(info *persistence.DomainInfo, config *persistence.DomainConfig) (*gen.DomainInfo,
	*gen.DomainConfiguration) {

	i := gen.NewDomainInfo()
	i.Name = common.StringPtr(info.Name)
	i.Status = getDomainStatus(info)
	i.Description = common.StringPtr(info.Description)
	i.OwnerEmail = common.StringPtr(info.OwnerEmail)

	c := gen.NewDomainConfiguration()
	c.EmitMetric = common.BoolPtr(config.EmitMetric)
	c.WorkflowExecutionRetentionPeriodInDays = common.Int32Ptr(config.Retention)

	return i, c
}

func createPollForDecisionTaskResponse(
	matchingResponse *m.PollForDecisionTaskResponse, history *gen.History, nextPageToken []byte) *gen.PollForDecisionTaskResponse {
	resp := gen.NewPollForDecisionTaskResponse()
	if matchingResponse != nil {
		resp.TaskToken = matchingResponse.TaskToken
		resp.WorkflowExecution = matchingResponse.WorkflowExecution
		resp.WorkflowType = matchingResponse.WorkflowType
		resp.PreviousStartedEventId = matchingResponse.PreviousStartedEventId
		resp.StartedEventId = matchingResponse.StartedEventId
	}
	resp.History = history
	resp.NextPageToken = nextPageToken
	return resp
}

func createGetWorkflowExecutionHistoryResponse(
	history *gen.History, nextEventID int64, nextPageToken []byte) *gen.GetWorkflowExecutionHistoryResponse {
	resp := gen.NewGetWorkflowExecutionHistoryResponse()
	resp.History = history
	resp.NextPageToken = nextPageToken
	return resp
}

func deserializeGetHistoryToken(data []byte) (*getHistoryContinuationToken, error) {
	var token getHistoryContinuationToken
	err := json.Unmarshal(data, &token)

	return &token, err
}

func getSerializedGetHistoryToken(persistenceToken []byte, runID string, history *gen.History, nextEventID int64) ([]byte, error) {
	// create token if there are more events to read
	if history == nil {
		return nil, nil
	}
	events := history.GetEvents()
	if len(persistenceToken) > 0 && len(events) > 0 && events[len(events)-1].GetEventId() < nextEventID-1 {
		token := &getHistoryContinuationToken{
			runID:            runID,
			nextEventID:      nextEventID,
			persistenceToken: persistenceToken,
		}
		data, err := json.Marshal(token)

		return data, err
	}
	return nil, nil
}
