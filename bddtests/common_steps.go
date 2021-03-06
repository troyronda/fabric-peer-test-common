/*
Copyright SecureKey Technologies Inc. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package bddtests

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/DATA-DOG/godog"
	"github.com/hyperledger/fabric-protos-go/common"
	fabricCommon "github.com/hyperledger/fabric-protos-go/common"
	"github.com/hyperledger/fabric-sdk-go/pkg/client/channel"
	"github.com/hyperledger/fabric-sdk-go/pkg/client/channel/invoke"
	"github.com/hyperledger/fabric-sdk-go/pkg/client/resmgmt"
	"github.com/hyperledger/fabric-sdk-go/pkg/common/errors/retry"
	"github.com/hyperledger/fabric-sdk-go/pkg/common/errors/status"
	"github.com/hyperledger/fabric-sdk-go/pkg/common/logging"
	contextApi "github.com/hyperledger/fabric-sdk-go/pkg/common/providers/context"
	fabApi "github.com/hyperledger/fabric-sdk-go/pkg/common/providers/fab"
	mspApi "github.com/hyperledger/fabric-sdk-go/pkg/common/providers/msp"
	contextImpl "github.com/hyperledger/fabric-sdk-go/pkg/context"
	"github.com/hyperledger/fabric-sdk-go/pkg/fab/ccpackager/gopackager"
	"github.com/hyperledger/fabric-sdk-go/third_party/github.com/hyperledger/fabric/common/cauthdsl"
	"github.com/pkg/errors"
	"github.com/tidwall/gjson"
)

// CommonSteps contain BDDContext
type CommonSteps struct {
	BDDContext *BDDContext
}

type Peers []*PeerConfig

func (p Peers) Shuffle() Peers {
	var peers Peers
	for _, i := range rand.Perm(len(p)) {
		peers = append(peers, p[i])
	}
	return peers
}

var logger = logging.NewLogger("test-logger")

var queryValue string
var vars = make(map[string]string)

type queryInfoResponse struct {
	Height            string
	CurrentBlockHash  string
	PreviousBlockHash string
}

var ccCodesForRetry = []int32{404}

// NewCommonSteps create new CommonSteps struct
func NewCommonSteps(context *BDDContext) *CommonSteps {
	//grpclog.SetLogger(logger)
	return &CommonSteps{BDDContext: context}
}

// GetDeployPath ..
func (d *CommonSteps) getDeployPath(ccType string) string {
	// test cc come from fixtures
	pwd, _ := os.Getwd()

	switch ccType {
	case "test":
		return path.Join(pwd, d.BDDContext.testCCPath)
	case "system":
		return path.Join(pwd, d.BDDContext.systemCCPath)
	default:
		panic(fmt.Sprintf("unsupported chaincode type: [%s]", ccType))
	}
}

func (d *CommonSteps) displayBlockFromChannel(blockNum int, channelID string) error {
	block, err := d.getBlocks(channelID, blockNum, 1)
	if err != nil {
		return err
	}
	logger.Infof("%s\n", block)
	return nil
}

func (d *CommonSteps) getBlocks(channelID string, blockNum, numBlocks int) (string, error) {
	orgID, err := d.BDDContext.OrgIDForChannel(channelID)
	if err != nil {
		return "", err
	}

	strBlockNum := fmt.Sprintf("%d", blockNum)
	strNumBlocks := fmt.Sprintf("%d", numBlocks)
	return NewFabCLI().Exec("query", "block", "--config", d.BDDContext.clientConfigFilePath+d.BDDContext.clientConfigFileName, "--cid", channelID, "--orgid", orgID, "--num", strBlockNum, "--traverse", strNumBlocks)
}

func (d *CommonSteps) displayBlocksFromChannel(numBlocks int, channelID string) error {
	height, err := d.getChannelBlockHeight(channelID)
	if err != nil {
		return fmt.Errorf("error getting channel height: %s", err)
	}

	block, err := d.getBlocks(channelID, height-1, numBlocks)
	if err != nil {
		return err
	}

	logger.Infof("%s\n", block)

	return nil
}

func (d *CommonSteps) getChannelBlockHeight(channelID string) (int, error) {
	orgID, err := d.BDDContext.OrgIDForChannel(channelID)
	if err != nil {
		return 0, err
	}

	resp, err := NewFabCLI().GetJSON("query", "info", "--config", d.BDDContext.clientConfigFilePath+d.BDDContext.clientConfigFileName, "--cid", channelID, "--orgid", orgID)
	if err != nil {
		return 0, err
	}

	var info queryInfoResponse
	if err := json.Unmarshal([]byte(resp), &info); err != nil {
		return 0, fmt.Errorf("Error unmarshalling JSON response: %s", err)
	}

	return strconv.Atoi(info.Height)
}

func (d *CommonSteps) displayLastBlockFromChannel(channelID string) error {
	return d.displayBlocksFromChannel(1, channelID)
}

func (d *CommonSteps) wait(seconds int) error {
	logger.Infof("Waiting [%d] seconds\n", seconds)
	time.Sleep(time.Duration(seconds) * time.Second)
	return nil
}

func (d *CommonSteps) createChannelAndJoinAllPeers(channelID string) error {
	return d.createChannelAndJoinPeers(channelID, d.BDDContext.Orgs())
}

func (d *CommonSteps) createChannelAndJoinPeersFromOrg(channelID, orgs string) error {
	orgList := strings.Split(orgs, ",")
	if len(orgList) == 0 {
		return fmt.Errorf("must specify at least one org ID")
	}
	return d.createChannelAndJoinPeers(channelID, orgList)
}

func (d *CommonSteps) createChannelAndJoinPeers(channelID string, orgs []string) error {
	logger.Infof("Creating channel [%s] and joining all peers from orgs %s", channelID, orgs)
	if len(orgs) == 0 {
		return fmt.Errorf("no orgs specified")
	}

	for _, orgID := range orgs {
		peersConfig, ok := d.BDDContext.clientConfig.PeersConfig(orgID)
		if !ok {
			return fmt.Errorf("could not get peers config for org [%s]", orgID)
		}
		if len(peersConfig) == 0 {
			return fmt.Errorf("no peers for org [%s]", orgID)
		}
		if err := d.joinPeersToChannel(channelID, orgID, peersConfig); err != nil {
			return fmt.Errorf("error joining peer to channel: %s", err)
		}

	}

	return nil
}

func (d *CommonSteps) joinPeersToChannel(channelID, orgID string, peersConfig []fabApi.PeerConfig) error {

	for _, peerConfig := range peersConfig {
		serverHostOverride := ""
		if str, ok := peerConfig.GRPCOptions["ssl-target-name-override"].(string); ok {
			serverHostOverride = str
		}
		d.BDDContext.AddPeerConfigToChannel(&PeerConfig{Config: peerConfig, OrgID: orgID, MspID: d.BDDContext.peersMspID[serverHostOverride], PeerID: serverHostOverride}, channelID)
	}
	peer, err := d.BDDContext.OrgUserContext(orgID, ADMIN).InfraProvider().CreatePeerFromConfig(&fabApi.NetworkPeer{PeerConfig: peersConfig[0]})
	if err != nil {
		return errors.WithMessage(err, "NewPeer failed")
	}
	resourceMgmt := d.BDDContext.ResMgmtClient(orgID, ADMIN)

	// Check if primary peer has joined channel
	alreadyJoined, err := HasPrimaryPeerJoinedChannel(channelID, resourceMgmt, d.BDDContext.OrgUserContext(orgID, ADMIN), peer)
	if err != nil {
		return fmt.Errorf("Error while checking if primary peer has already joined channel: %s", err)
	} else if alreadyJoined {
		logger.Infof("alreadyJoined orgID [%s]\n", orgID)
		return nil
	}

	if d.BDDContext.ChannelCreated(channelID) == false {
		// only the first peer of the first org can create a channel
		logger.Infof("Creating channel [%s]\n", channelID)
		txPath := GetChannelTxPath(channelID)
		if txPath == "" {
			return fmt.Errorf("channel TX path not found for channel: %s", channelID)
		}

		// Create and join channel
		req := resmgmt.SaveChannelRequest{ChannelID: channelID,
			ChannelConfigPath: txPath,
			SigningIdentities: []mspApi.SigningIdentity{d.BDDContext.OrgUserContext(orgID, ADMIN)}}

		if _, err = resourceMgmt.SaveChannel(req, resmgmt.WithRetry(retry.DefaultResMgmtOpts)); err != nil {
			return errors.WithMessage(err, "SaveChannel failed")
		}
	}

	logger.Infof("Updating anchor peers for org [%s] on channel [%s]\n", orgID, channelID)

	// Update anchors for peer org
	anchorTxPath := GetChannelAnchorTxPath(channelID, orgID)
	if anchorTxPath == "" {
		return fmt.Errorf("anchor TX path not found for channel [%s] and org [%s]", channelID, orgID)
	}
	// Create channel (or update if it already exists)
	req := resmgmt.SaveChannelRequest{ChannelID: channelID,
		ChannelConfigPath: anchorTxPath,
		SigningIdentities: []mspApi.SigningIdentity{d.BDDContext.OrgUserContext(orgID, ADMIN)}}

	if _, err := resourceMgmt.SaveChannel(req, resmgmt.WithRetry(retry.DefaultResMgmtOpts)); err != nil {
		return errors.WithMessage(err, "SaveChannel failed")
	}

	d.BDDContext.createdChannels[channelID] = true

	// Join Channel without error for anchor peers only. ignore JoinChannel error for other peers as AnchorePeer with JoinChannel will add all org's peers

	resMgmtClient := d.BDDContext.ResMgmtClient(orgID, ADMIN)
	if err = resMgmtClient.JoinChannel(channelID, resmgmt.WithRetry(retry.DefaultResMgmtOpts)); err != nil {
		return fmt.Errorf("JoinChannel returned error: %s", err)
	}

	return nil
}

// InvokeCConOrg invoke cc on org
func (d *CommonSteps) InvokeCConOrg(ccID, args, orgIDs, channelID string) error {
	argArr, err := ResolveAllVars(args)
	if err != nil {
		return err
	}
	if _, err := d.InvokeCCWithArgs(ccID, channelID, d.OrgPeers(orgIDs, channelID), argArr, nil); err != nil {
		return fmt.Errorf("InvokeCCWithArgs return error: %s", err)
	}
	return nil
}

// InvokeCC invoke cc
func (d *CommonSteps) InvokeCC(ccID, args, channelID string) error {
	argArr, err := ResolveAllVars(args)
	if err != nil {
		return err
	}
	if _, err := d.InvokeCCWithArgs(ccID, channelID, nil, argArr, nil); err != nil {
		return fmt.Errorf("InvokeCC return error: %s", err)
	}
	return nil
}

//InvokeCCWithArgsAsAdmin invoke cc with args as admin user type
func (d *CommonSteps) InvokeCCWithArgsAsAdmin(ccID, channelID string, targets []*PeerConfig, args []string, transientData map[string][]byte) (channel.Response, error) {
	return d.invokeCCWithArgs(ccID, channelID, targets, args, transientData, ADMIN)
}

//InvokeCCWithArgs invoke cc with args as regular user
func (d *CommonSteps) InvokeCCWithArgs(ccID, channelID string, targets []*PeerConfig, args []string, transientData map[string][]byte) (channel.Response, error) {
	return d.invokeCCWithArgs(ccID, channelID, targets, args, transientData, USER)
}

// invokeCCWithArgs ...
func (d *CommonSteps) invokeCCWithArgs(ccID, channelID string, targets []*PeerConfig, args []string, transientData map[string][]byte, userType string) (channel.Response, error) {
	var peers []fabApi.Peer

	for _, target := range targets {
		targetPeer, err := d.BDDContext.OrgUserContext(targets[0].OrgID, ADMIN).InfraProvider().CreatePeerFromConfig(&fabApi.NetworkPeer{PeerConfig: target.Config})
		if err != nil {
			return channel.Response{}, errors.WithMessage(err, "NewPeer failed")
		}
		peers = append(peers, targetPeer)
	}

	chClient, err := d.BDDContext.OrgChannelClient(d.BDDContext.orgs[0], userType, channelID)
	if err != nil {
		return channel.Response{}, fmt.Errorf("Failed to create new channel client: %s", err)
	}

	retryOpts := retry.DefaultOpts
	retryOpts.RetryableCodes = retry.ChannelClientRetryableCodes

	for _, code := range ccCodesForRetry {
		addRetryCode(retryOpts.RetryableCodes, status.ChaincodeStatus, status.Code(code))
	}

	response, err := chClient.Execute(
		channel.Request{
			ChaincodeID: ccID,
			Fcn:         args[0],
			Args:        GetByteArgs(args[1:]),
		},
		channel.WithTargets(peers...),
		channel.WithRetry(retryOpts),
	)

	if err != nil {
		return channel.Response{}, fmt.Errorf("InvokeChaincode return error: %s", err)
	}
	return response, nil
}

// addRetryCode adds the given group and code to the given map
func addRetryCode(codes map[status.Group][]status.Code, group status.Group, code status.Code) {
	g, exists := codes[group]
	if !exists {
		g = []status.Code{}
	}
	codes[group] = append(g, code)
}

func (d *CommonSteps) queryCConOrg(ccID, args, orgIDs, channelID string) error {
	queryValue = ""

	argArr, err := ResolveAllVars(args)
	if err != nil {
		return err
	}

	queryValue, err = d.QueryCCWithArgs(false, ccID, channelID, argArr, nil, d.OrgPeers(orgIDs, channelID)...)
	if err != nil {
		return fmt.Errorf("QueryCCWithArgs return error: %s", err)
	}
	logger.Debugf("QueryCCWithArgs return value: [%s]", queryValue)
	return nil
}

func (d *CommonSteps) queryCConTargetPeers(ccID, args, peerIDs, channelID string) error {
	queryValue = ""

	if peerIDs == "" {
		return errors.New("no target peers specified")
	}

	targetPeers, err := d.Peers(peerIDs)
	if err != nil {
		return err
	}

	logger.Debugf("Querying peers [%s]...", targetPeers)

	argArr, err := ResolveAllVars(args)
	if err != nil {
		return err
	}

	queryValue, err = d.QueryCCWithArgs(false, ccID, channelID, argArr, nil, targetPeers...)
	if err != nil {
		return fmt.Errorf("QueryCCWithArgs return error: %s", err)
	}
	logger.Debugf("QueryCCWithArgs return value: [%s]", queryValue)
	return nil
}

func (d *CommonSteps) invokeCConTargetPeers(ccID, args, peerIDs, channelID string) error {
	queryValue = ""

	if peerIDs == "" {
		return errors.New("no target peers specified")
	}

	targetPeers, err := d.Peers(peerIDs)
	if err != nil {
		return err
	}

	logger.Debugf("Invoking peers [%s]...", targetPeers)

	argArr, err := ResolveAllVars(args)
	if err != nil {
		return err
	}

	resp, err := d.InvokeCCWithArgs(ccID, channelID, targetPeers, argArr, nil)
	if err != nil {
		return fmt.Errorf("InvokeCCWithArgs returned error: %s", err)
	}
	queryValue = string(resp.Payload)
	logger.Debugf("InvokeCCWithArgs returned value: [%s]", queryValue)
	return nil
}

func (d *CommonSteps) queryCConSinglePeerInOrg(ccID, args, orgIDs, channelID string) error {
	queryValue = ""

	targetPeers := d.OrgPeers(orgIDs, channelID)
	if len(targetPeers) == 0 {
		return errors.Errorf("no peers in org(s) [%s] for channel [%s]", orgIDs, channelID)
	}

	// Pick a random peer
	targetPeer := targetPeers.Shuffle()[0]

	logger.Infof("Querying peer [%s]...", targetPeer.Config.URL)

	argArr, err := ResolveAllVars(args)
	if err != nil {
		return err
	}

	queryValue, err = d.QueryCCWithArgs(false, ccID, channelID, argArr, nil, targetPeer)
	if err != nil {
		return fmt.Errorf("QueryCCWithArgs return error: %s", err)
	}
	logger.Debugf("QueryCCWithArgs return value: [%s]", queryValue)
	return nil
}

func (d *CommonSteps) querySystemCC(ccID, args, orgID, channelID string) error {
	queryValue = ""

	peersConfig, ok := d.BDDContext.clientConfig.PeersConfig(orgID)
	if !ok {
		return fmt.Errorf("could not get peers config for org [%s]", orgID)
	}

	serverHostOverride := ""
	if str, ok := peersConfig[0].GRPCOptions["ssl-target-name-override"].(string); ok {
		serverHostOverride = str
	}

	argsArray, err := ResolveAllVars(args)
	if err != nil {
		return err
	}

	queryValue, err = d.QueryCCWithArgs(true, ccID, channelID, argsArray, nil,
		[]*PeerConfig{{Config: peersConfig[0], OrgID: orgID, MspID: d.BDDContext.peersMspID[serverHostOverride], PeerID: serverHostOverride}}...)
	if err != nil {
		return fmt.Errorf("QueryCCWithArgs return error: %s", err)
	}
	logger.Debugf("QueryCCWithArgs return value: [%s]", queryValue)
	return nil
}

func (d *CommonSteps) queryCC(ccID, args, channelID string) error {
	logger.Infof("Querying chaincode [%s] on channel [%s] with args [%s]", ccID, channelID, args)

	queryValue = ""

	argArr, err := ResolveAllVars(args)
	if err != nil {
		return err
	}

	queryValue, err = d.QueryCCWithArgs(false, ccID, channelID, argArr, nil)
	if err != nil {
		return fmt.Errorf("QueryCCWithArgs return error: %s", err)
	}
	logger.Infof("QueryCC return value: [%s]", queryValue)
	return nil
}

func (d *CommonSteps) queryCCWithError(ccID, args, channelID string, expectedError string) error {
	err := d.queryCC(ccID, args, channelID)
	if err == nil {
		return errors.Errorf("expecting error [%s] but got no error", expectedError)
	}

	if !strings.Contains(err.Error(), expectedError) {
		return errors.Errorf("expecting error [%s] but got [%s]", expectedError, err)
	}

	return nil
}

// QueryCCWithArgs ...
func (d *CommonSteps) QueryCCWithArgs(systemCC bool, ccID, channelID string, args []string, transientData map[string][]byte, targets ...*PeerConfig) (string, error) {
	return d.QueryCCWithOpts(systemCC, ccID, channelID, args, 0, true, 0, transientData, targets...)
}

// QueryCCWithOpts ...
func (d *CommonSteps) QueryCCWithOpts(systemCC bool, ccID, channelID string, args []string, timeout time.Duration, concurrent bool, interval time.Duration, transientData map[string][]byte, targets ...*PeerConfig) (string, error) {
	var peers []fabApi.Peer
	var orgID string
	var queryResult string
	for _, target := range targets {
		orgID = target.OrgID

		targetPeer, err := d.BDDContext.OrgUserContext(orgID, ADMIN).InfraProvider().CreatePeerFromConfig(&fabApi.NetworkPeer{PeerConfig: target.Config})
		if err != nil {
			return "", errors.WithMessage(err, "NewPeer failed")
		}

		peers = append(peers, targetPeer)
	}

	if orgID == "" {
		orgID = d.BDDContext.orgs[0]
	}

	chClient, err := d.BDDContext.OrgChannelClient(orgID, ADMIN, channelID)
	if err != nil {
		logger.Errorf("Failed to create new channel client: %s", err)
		return "", errors.Wrap(err, "Failed to create new channel client")
	}

	retryOpts := retry.DefaultOpts
	retryOpts.RetryableCodes = retry.ChannelClientRetryableCodes

	for _, code := range ccCodesForRetry {
		addRetryCode(retryOpts.RetryableCodes, status.ChaincodeStatus, status.Code(code))
	}

	if systemCC {
		// Create a system channel client

		systemHandlerChain := invoke.NewProposalProcessorHandler(
			NewCustomEndorsementHandler(
				d.BDDContext.OrgUserContext(orgID, USER),
				invoke.NewEndorsementValidationHandler(),
			))

		resp, err := chClient.InvokeHandler(systemHandlerChain, channel.Request{
			ChaincodeID:  ccID,
			Fcn:          args[0],
			Args:         GetByteArgs(args[1:]),
			TransientMap: transientData,
		}, channel.WithTargets(peers...), channel.WithTimeout(fabApi.Execute, timeout), channel.WithRetry(retryOpts))
		if err != nil {
			return "", fmt.Errorf("QueryChaincode return error: %s", err)
		}
		queryResult = string(resp.Payload)
		return queryResult, nil
	}

	if concurrent {

		resp, err := chClient.Query(channel.Request{
			ChaincodeID:  ccID,
			Fcn:          args[0],
			Args:         GetByteArgs(args[1:]),
			TransientMap: transientData,
		}, channel.WithTargets(peers...), channel.WithTimeout(fabApi.Execute, timeout), channel.WithRetry(retryOpts))
		if err != nil {
			return "", fmt.Errorf("QueryChaincode return error: %s", err)
		}
		queryResult = string(resp.Payload)

	} else {
		var errs []error
		for _, peer := range peers {
			if len(args) > 0 && args[0] == "warmup" {
				logger.Infof("Warming up chaincode [%s] on peer [%s] in channel [%s]", ccID, peer.URL(), channelID)
			}
			resp, err := chClient.Query(channel.Request{
				ChaincodeID:  ccID,
				Fcn:          args[0],
				Args:         GetByteArgs(args[1:]),
				TransientMap: transientData,
			}, channel.WithTargets([]fabApi.Peer{peer}...), channel.WithTimeout(fabApi.Execute, timeout), channel.WithRetry(retryOpts))
			if err != nil {
				errs = append(errs, err)
			} else {
				queryResult = string(resp.Payload)
			}
			if interval > 0 {
				logger.Infof("Waiting %s\n", interval)
				time.Sleep(interval)
			}
		}
		if len(errs) > 0 {
			return "", fmt.Errorf("QueryChaincode return error: %s", errs[0])
		}
	}

	logger.Debugf("QueryChaincode return value: [%s]", queryResult)
	return queryResult, nil
}

func (d *CommonSteps) containsInQueryValue(ccID string, value string) error {
	logger.Infof("Query value %s and tested value %s", queryValue, value)
	if !strings.Contains(queryValue, value) {
		return fmt.Errorf("Query value(%s) doesn't contain expected value(%s)", queryValue, value)
	}
	return nil
}

func (d *CommonSteps) equalQueryValue(ccID string, value string) error {
	logger.Infof("Query value %s and tested value %s", queryValue, value)
	if queryValue == value {
		return nil
	}
	return fmt.Errorf("Query value(%s) doesn't equal expected value(%s)", queryValue, value)
}

func (d *CommonSteps) setVariableFromCCResponse(key string) error {
	logger.Infof("Saving value %s to variable %s", queryValue, key)
	SetVar(key, queryValue)
	return nil
}

func (d *CommonSteps) setJSONVariable(varName, value string) error {
	// Validate the JSON
	m := make(map[string]interface{})
	if err := json.Unmarshal([]byte(value), &m); err != nil {
		return errors.WithMessagef(err, "invalid JSON: %s", value)
	}
	SetVar(varName, value)
	return nil
}

func (d *CommonSteps) jsonPathOfCCResponseEquals(path, expected string) error {
	r := gjson.Get(queryValue, path)
	logger.Infof("Path [%s] of JSON %s resolves to %s", path, queryValue, r.Str)
	if r.Str == expected {
		return nil
	}
	return fmt.Errorf("JSON path resolves to [%s] which is not the expected value [%s]", r.Str, expected)
}

func (d *CommonSteps) jsonPathOfCCHasNumItems(path string, expectedNum int) error {
	r := gjson.Get(queryValue, path)
	logger.Infof("Path [%s] of JSON %s resolves to %d items", path, queryValue, int(r.Num))
	if int(r.Num) == expectedNum {
		return nil
	}
	return fmt.Errorf("JSON path resolves to [%d] items which is not the expected number of items [%d]", int(r.Num), expectedNum)
}

func (d *CommonSteps) jsonPathOfCCResponseContains(path, expected string) error {
	r := gjson.Get(queryValue, path)
	logger.Infof("Path [%s] of JSON %s resolves to %s", path, queryValue, r.Raw)
	for _, a := range r.Array() {
		if a.Str == expected {
			return nil
		}
	}
	return fmt.Errorf("JSON path resolves to [%s] which is not the expected value [%s]", r.Array(), expected)
}

func (d *CommonSteps) installChaincodeToAllPeers(ccType, ccID, ccPath string) error {
	logger.Infof("Installing chaincode [%s] from path [%s] to all peers", ccID, ccPath)
	return d.doInstallChaincodeToOrg(ccType, ccID, ccPath, "v1", "", "")
}

func (d *CommonSteps) installChaincodeToAllPeersWithVersion(ccType, ccID, ccVersion, ccPath string) error {
	logger.Infof("Installing chaincode [%s:%s] from path [%s] to all peers", ccID, ccVersion, ccPath)
	return d.doInstallChaincodeToOrg(ccType, ccID, ccPath, ccVersion, "", "")
}

func (d *CommonSteps) installChaincodeToAllPeersExcept(ccType, ccID, ccPath, blackListRegex string) error {
	logger.Infof("Installing chaincode [%s] from path [%s] to all peers except [%s]", ccID, ccPath, blackListRegex)
	return d.doInstallChaincodeToOrg(ccType, ccID, ccPath, "v1", "", blackListRegex)
}

func (d *CommonSteps) instantiateChaincode(ccType, ccID, ccPath, channelID, args, ccPolicy, collectionNames string) error {
	logger.Infof("Preparing to instantiate chaincode [%s] from path [%s] on channel [%s] with args [%s] and CC policy [%s] and collectionPolicy [%s]", ccID, ccPath, channelID, args, ccPolicy, collectionNames)
	return d.instantiateChaincodeWithOpts(ccType, ccID, ccPath, "", channelID, args, ccPolicy, collectionNames, false)
}

func (d *CommonSteps) upgradeChaincode(ccType, ccID, ccVersion, ccPath, channelID, args, ccPolicy, collectionNames string) error {
	logger.Infof("Preparing to instantiate chaincode [%s] from path [%s] on channel [%s] with args [%s] and CC policy [%s] and collectionPolicy [%s]", ccID, ccPath, channelID, args, ccPolicy, collectionNames)
	return d.upgradeChaincodeWithOpts(ccType, ccID, ccVersion, ccPath, "", channelID, args, ccPolicy, collectionNames, false)
}

func (d *CommonSteps) upgradeChaincodeWithError(ccType, ccID, ccVersion, ccPath, channelID, args, ccPolicy, collectionNames, expectedError string) error {
	logger.Infof("Preparing to instantiate chaincode [%s] from path [%s] on channel [%s] with args [%s] and CC policy [%s] and collectionPolicy [%s]. Expected error [%s]", ccID, ccPath, channelID, args, ccPolicy, collectionNames, expectedError)
	err := d.upgradeChaincodeWithOpts(ccType, ccID, ccVersion, ccPath, "", channelID, args, ccPolicy, collectionNames, false)
	if err == nil {
		return errors.Errorf("expecting error [%s] but got no error", expectedError)
	}
	if !strings.Contains(err.Error(), expectedError) {
		return errors.Errorf("expecting error [%s] but got [%s]", expectedError, err)
	}
	return nil
}

func (d *CommonSteps) instantiateChaincodeOnOrg(ccType, ccID, ccPath, orgIDs, channelID, args, ccPolicy, collectionNames string) error {
	logger.Infof("Preparing to instantiate chaincode [%s] from path [%s] to orgs [%s] on channel [%s] with args [%s] and CC policy [%s] and collectionPolicy [%s]", ccID, ccPath, orgIDs, channelID, args, ccPolicy, collectionNames)
	return d.instantiateChaincodeWithOpts(ccType, ccID, ccPath, orgIDs, channelID, args, ccPolicy, collectionNames, false)
}

func (d *CommonSteps) deployChaincode(ccType, ccID, ccPath, channelID, args, ccPolicy, collectionPolicy string) error {
	logger.Infof("Installing and instantiating chaincode [%s] from path [%s] to channel [%s] with args [%s] and CC policy [%s] and collectionPolicy [%s]", ccID, ccPath, channelID, args, ccPolicy, collectionPolicy)
	return d.deployChaincodeToOrg(ccType, ccID, ccPath, "", channelID, args, ccPolicy, collectionPolicy)
}

func (d *CommonSteps) installChaincodeToOrg(ccType, ccID, ccPath, orgIDs string) error {
	return d.doInstallChaincodeToOrg(ccType, ccID, ccPath, "v1", orgIDs, "")
}

func (d *CommonSteps) doInstallChaincodeToOrg(ccType, ccID, ccPath, ccVersion, orgIDs, blackListRegex string) error {
	logger.Infof("Preparing to install chaincode [%s:%s] from path [%s] to orgs [%s] - Blacklisted peers: [%s]", ccID, ccPath, ccVersion, orgIDs, blackListRegex)

	var oIDs []string
	if orgIDs != "" {
		oIDs = strings.Split(orgIDs, ",")
	} else {
		oIDs = d.BDDContext.orgs
	}

	for _, orgID := range oIDs {
		targets, err := d.getLocalTargets(orgID, blackListRegex)
		if err != nil {
			return err
		}

		resMgmtClient := d.BDDContext.ResMgmtClient(orgID, ADMIN)

		ccPkg, err := gopackager.NewCCPackage(ccPath, d.getDeployPath(ccType))
		if err != nil {
			return err
		}

		if len(targets) == 0 {
			return errors.Errorf("no targets for chaincode [%s]", ccID)
		}

		logger.Infof("... installing chaincode [%s] from path [%s] to targets %s", ccID, ccPath, targets)
		_, err = resMgmtClient.InstallCC(
			resmgmt.InstallCCRequest{Name: ccID, Path: ccPath, Version: ccVersion, Package: ccPkg},
			resmgmt.WithRetry(retry.DefaultResMgmtOpts),
			resmgmt.WithTargetEndpoints(targets...),
		)
		if err != nil {
			return fmt.Errorf("SendInstallProposal return error: %s", err)
		}
	}
	return nil
}

func (d *CommonSteps) getLocalTargets(orgID string, blackListRegex string) ([]string, error) {
	return getLocalTargets(d.BDDContext, orgID, blackListRegex)
}

func getLocalTargets(context *BDDContext, orgID string, blackListRegex string) ([]string, error) {
	var blacklistedPeersRegex *regexp.Regexp
	if blackListRegex != "" {
		var err error
		blacklistedPeersRegex, err = regexp.Compile(blackListRegex)
		if err != nil {
			return nil, err
		}
	}

	contextProvider := func() (contextApi.Client, error) {
		return context.OrgUserContext(orgID, ADMIN), nil
	}

	localContext, err := contextImpl.NewLocal(contextProvider)
	if err != nil {
		return nil, err
	}

	peers, err := localContext.LocalDiscoveryService().GetPeers()
	if err != nil {
		return nil, err
	}

	var peerURLs []string
	for _, peer := range peers {
		peerConfig := context.PeerConfigForURL(peer.URL())
		if peerConfig == nil {
			logger.Warnf("Peer config not found for URL [%s]", peer.URL())
			continue
		}
		if blacklistedPeersRegex != nil && blacklistedPeersRegex.MatchString(peerConfig.PeerID) {
			logger.Infof("Not returning local peer [%s] since it is blacklisted", peerConfig.PeerID)
			continue
		}
		peerURLs = append(peerURLs, peer.URL())
	}

	return peerURLs, nil
}

func (d *CommonSteps) instantiateChaincodeWithOpts(ccType, ccID, ccPath, orgIDs, channelID, args, ccPolicy, collectionNames string, allPeers bool) error {
	logger.Infof("Preparing to instantiate chaincode [%s] from path [%s] to orgs [%s] on channel [%s] with args [%s] and CC policy [%s] and collectionPolicy [%s]", ccID, ccPath, orgIDs, channelID, args, ccPolicy, collectionNames)

	peers := d.OrgPeers(orgIDs, channelID)
	if len(peers) == 0 {
		return errors.Errorf("no peers found for orgs [%s]", orgIDs)
	}
	chaincodePolicy, err := d.newChaincodePolicy(ccPolicy, channelID)
	if err != nil {
		return fmt.Errorf("error creating endorsement policy: %s", err)
	}

	var sdkPeers []fabApi.Peer
	var orgID string

	for _, pconfig := range peers {
		orgID = pconfig.OrgID

		sdkPeer, err := d.BDDContext.OrgUserContext(orgID, ADMIN).InfraProvider().CreatePeerFromConfig(&fabApi.NetworkPeer{PeerConfig: pconfig.Config})
		if err != nil {
			return errors.WithMessage(err, "NewPeer failed")
		}

		sdkPeers = append(sdkPeers, sdkPeer)
		if !allPeers {
			break
		}
	}

	var collConfig []*common.CollectionConfig
	if collectionNames != "" {
		// Define the private data collection policy config
		for _, collName := range strings.Split(collectionNames, ",") {
			logger.Infof("Configuring collection (%s) for CCID=%s", collName, ccID)
			c, err := d.newCollectionConfig(channelID, collName)
			if err != nil {
				return err
			}
			collConfig = append(collConfig, c)
		}
	}

	resMgmtClient := d.BDDContext.ResMgmtClient(orgID, ADMIN)

	logger.Infof("Instantiating chaincode [%s] from path [%s] on channel [%s] with args [%s] and CC policy [%s] and collectionPolicy [%s] to the following peers: [%s]", ccID, ccPath, channelID, args, ccPolicy, collectionNames, peersAsString(sdkPeers))

	_, err = resMgmtClient.InstantiateCC(
		channelID,
		resmgmt.InstantiateCCRequest{
			Name:       ccID,
			Path:       ccPath,
			Version:    "v1",
			Args:       GetByteArgs(strings.Split(args, ",")),
			Policy:     chaincodePolicy,
			CollConfig: collConfig,
		},
		resmgmt.WithTargets(sdkPeers...),
		resmgmt.WithTimeout(fabApi.Execute, 5*time.Minute),
		resmgmt.WithRetry(retry.DefaultResMgmtOpts),
	)

	if err != nil && strings.Contains(err.Error(), "already exists") {
		logger.Warnf("error from InstantiateCC %v", err)
		return nil
	}
	return err
}

func (d *CommonSteps) upgradeChaincodeWithOpts(ccType, ccID, ccVersion, ccPath, orgIDs, channelID, args, ccPolicy, collectionNames string, allPeers bool) error {
	logger.Infof("Preparing to upgrade chaincode [%s:%s] from path [%s] to orgs [%s] on channel [%s] with args [%s] and CC policy [%s] and collectionPolicy [%s]", ccID, ccVersion, ccPath, orgIDs, channelID, args, ccPolicy, collectionNames)

	peers := d.OrgPeers(orgIDs, channelID)
	if len(peers) == 0 {
		return errors.Errorf("no peers found for orgs [%s]", orgIDs)
	}
	chaincodePolicy, err := d.newChaincodePolicy(ccPolicy, channelID)
	if err != nil {
		return fmt.Errorf("error creating endorsement policy: %s", err)
	}

	var sdkPeers []fabApi.Peer
	var orgID string

	for _, pconfig := range peers {
		orgID = pconfig.OrgID

		sdkPeer, err := d.BDDContext.OrgUserContext(orgID, ADMIN).InfraProvider().CreatePeerFromConfig(&fabApi.NetworkPeer{PeerConfig: pconfig.Config})
		if err != nil {
			return errors.WithMessage(err, "NewPeer failed")
		}

		sdkPeers = append(sdkPeers, sdkPeer)
		if !allPeers {
			break
		}
	}

	var collConfig []*common.CollectionConfig
	if collectionNames != "" {
		// Define the private data collection policy config
		for _, collName := range strings.Split(collectionNames, ",") {
			logger.Infof("Configuring collection (%s) for CCID=%s", collName, ccID)
			c, err := d.newCollectionConfig(channelID, collName)
			if err != nil {
				return err
			}
			collConfig = append(collConfig, c)
		}
	}

	resMgmtClient := d.BDDContext.ResMgmtClient(orgID, ADMIN)

	logger.Infof("Upgrading chaincode [%s] from path [%s] on channel [%s] with args [%s] and CC policy [%s] and collectionPolicy [%s] to the following peers: [%s]", ccID, ccPath, channelID, args, ccPolicy, collectionNames, peersAsString(sdkPeers))

	_, err = resMgmtClient.UpgradeCC(
		channelID,
		resmgmt.UpgradeCCRequest{
			Name:       ccID,
			Path:       ccPath,
			Version:    ccVersion,
			Args:       GetByteArgs(strings.Split(args, ",")),
			Policy:     chaincodePolicy,
			CollConfig: collConfig,
		},
		resmgmt.WithTargets(sdkPeers...),
		resmgmt.WithTimeout(fabApi.Execute, 5*time.Minute),
		resmgmt.WithRetry(retry.DefaultResMgmtOpts),
	)

	if err != nil && strings.Contains(err.Error(), "already exists") {
		logger.Warnf("error from InstantiateCC %v", err)
		return nil
	}
	return err
}

func (d *CommonSteps) deployChaincodeToOrg(ccType, ccID, ccPath, orgIDs, channelID, args, ccPolicy, collectionNames string) error {
	logger.Infof("Installing and instantiating chaincode [%s] from path [%s] to orgs [%s] on channel [%s] with args [%s] and CC policy [%s] and collectionPolicy [%s]", ccID, ccPath, orgIDs, channelID, args, ccPolicy, collectionNames)

	peers := d.OrgPeers(orgIDs, channelID)
	if len(peers) == 0 {
		return errors.Errorf("no peers found for orgs [%s]", orgIDs)
	}
	chaincodePolicy, err := d.newChaincodePolicy(ccPolicy, channelID)
	if err != nil {
		return fmt.Errorf("error creating endirsement policy: %s", err)
	}

	var sdkPeers []fabApi.Peer
	var isInstalled bool
	var orgID string

	for _, pconfig := range peers {
		orgID = pconfig.OrgID

		sdkPeer, err := d.BDDContext.OrgUserContext(orgID, ADMIN).InfraProvider().CreatePeerFromConfig(&fabApi.NetworkPeer{PeerConfig: pconfig.Config})
		if err != nil {
			return errors.WithMessage(err, "NewPeer failed")
		}
		resourceMgmt := d.BDDContext.ResMgmtClient(orgID, ADMIN)
		isInstalled, err = IsChaincodeInstalled(resourceMgmt, sdkPeer, ccID)
		if err != nil {
			return fmt.Errorf("Error querying installed chaincodes: %s", err)
		}

		if !isInstalled {

			resMgmtClient := d.BDDContext.ResMgmtClient(orgID, ADMIN)
			ccPkg, err := gopackager.NewCCPackage(ccPath, d.getDeployPath(ccType))
			if err != nil {
				return err
			}

			installRqst := resmgmt.InstallCCRequest{Name: ccID, Path: ccPath, Version: "v1", Package: ccPkg}
			_, err = resMgmtClient.InstallCC(installRqst, resmgmt.WithRetry(retry.DefaultResMgmtOpts))
			if err != nil {
				return fmt.Errorf("SendInstallProposal return error: %s", err)
			}
		}

		sdkPeers = append(sdkPeers, sdkPeer)
	}

	argsArray := strings.Split(args, ",")

	var collConfig []*common.CollectionConfig
	if collectionNames != "" {
		// Define the private data collection policy config
		for _, collName := range strings.Split(collectionNames, ",") {
			logger.Infof("Configuring collection (%s) for CCID=%s", collName, ccID)
			c, err := d.newCollectionConfig(channelID, collName)
			if err != nil {
				return err
			}
			collConfig = append(collConfig, c)
		}
	}

	resMgmtClient := d.BDDContext.ResMgmtClient(orgID, ADMIN)

	instantiateRqst := resmgmt.InstantiateCCRequest{Name: ccID, Path: ccPath, Version: "v1", Args: GetByteArgs(argsArray), Policy: chaincodePolicy,
		CollConfig: collConfig}

	_, err = resMgmtClient.InstantiateCC(
		channelID, instantiateRqst,
		resmgmt.WithTargets(sdkPeers...),
		resmgmt.WithTimeout(fabApi.Execute, 5*time.Minute),
		resmgmt.WithRetry(retry.DefaultResMgmtOpts),
	)
	return err
}

func (d *CommonSteps) newChaincodePolicy(ccPolicy, channelID string) (*fabricCommon.SignaturePolicyEnvelope, error) {
	return NewChaincodePolicy(d.BDDContext, ccPolicy, channelID)
}

//OrgPeers return array of PeerConfig
func (d *CommonSteps) OrgPeers(orgIDs, channelID string) Peers {
	var orgMap map[string]bool
	if orgIDs != "" {
		orgMap = make(map[string]bool)
		for _, orgID := range strings.Split(orgIDs, ",") {
			orgMap[orgID] = true
		}
	}
	var peers []*PeerConfig
	for _, pconfig := range d.BDDContext.PeersByChannel(channelID) {
		if orgMap == nil || orgMap[pconfig.OrgID] {
			peers = append(peers, pconfig)
		}
	}
	return peers
}

// Peers returns the PeerConfigs for the given peer IDs
func (d *CommonSteps) Peers(peerIDs string) (Peers, error) {
	var peers []*PeerConfig
	for _, id := range strings.Split(peerIDs, ",") {
		peer := d.BDDContext.PeerConfigForID(id)
		if peer == nil {
			return nil, errors.Errorf("peer [%s] not found", id)
		}
		peers = append(peers, peer)
	}
	return peers, nil
}

func (d *CommonSteps) warmUpCC(ccID, channelID string) error {
	logger.Infof("Warming up chaincode [%s] on channel [%s]", ccID, channelID)
	return d.warmUpCConOrg(ccID, "", channelID)
}

func (d *CommonSteps) warmUpCConOrg(ccID, orgIDs, channelID string) error {
	logger.Infof("Warming up chaincode [%s] on orgs [%s] and channel [%s]", ccID, orgIDs, channelID)
	for {
		_, err := d.QueryCCWithOpts(false, ccID, channelID, []string{"warmup"}, 5*time.Minute, false, 0, nil, d.OrgPeers(orgIDs, channelID)...)
		if err != nil && strings.Contains(err.Error(), "premature execution - chaincode") {
			// Wait until we can successfully invoke the chaincode
			logger.Infof("Error warming up chaincode [%s]: %s. Retrying in 5 seconds...", ccID, err)
			time.Sleep(5 * time.Second)
		} else {
			// Don't worry about any other type of error
			return nil
		}
	}
}

func (d *CommonSteps) defineCollectionConfig(id, collection, policy string, requiredPeerCount int, maxPeerCount int, blocksToLive int) error {
	logger.Infof("Defining collection config [%s] for collection [%s] - policy=[%s], requiredPeerCount=[%d], maxPeerCount=[%d], blocksToLive=[%d]", id, collection, policy, requiredPeerCount, maxPeerCount, blocksToLive)
	d.DefineCollectionConfig(id, collection, policy, int32(requiredPeerCount), int32(maxPeerCount), uint64(blocksToLive))
	return nil
}

func (d *CommonSteps) newCollectionConfig(channelID string, collName string) (*common.CollectionConfig, error) {
	createCollectionConfig := d.BDDContext.CollectionConfig(collName)
	if createCollectionConfig == nil {
		return nil, errors.Errorf("no collection config defined for collection [%s]", collName)
	}
	return createCollectionConfig(channelID)
}

// DefineCollectionConfig defines a new private data collection configuration
func (d *CommonSteps) DefineCollectionConfig(id, name, policy string, requiredPeerCount, maxPeerCount int32, blocksToLive uint64) {
	d.BDDContext.DefineCollectionConfig(id,
		func(channelID string) (*common.CollectionConfig, error) {
			sigPolicy, err := d.newChaincodePolicy(policy, channelID)
			if err != nil {
				return nil, errors.Wrapf(err, "error creating collection policy for collection [%s]", name)
			}
			return newPrivateCollectionConfig(name, requiredPeerCount, maxPeerCount, blocksToLive, sigPolicy), nil
		},
	)
}

// ClearResponse clears the query response
func ClearResponse() {
	queryValue = ""
}

// GetResponse returns the most recent query response
func GetResponse() string {
	return queryValue
}

// SetResponse sets the query response
func SetResponse(response string) {
	queryValue = response
}

// SetVar sets the value for the given variable
func SetVar(varName, value string) {
	vars[varName] = value
}

// GetVar gets the value for the given variable
// Returns true if the variable exists; false otherwise
func GetVar(varName string) (string, bool) {
	value, ok := vars[varName]
	return value, ok
}

// ResolveAllVars returns a slice of strings from the given comma-separated string.
// Each string is resolved for variables.
// Resolve resolves all variables within the given arg
//
// Example 1: Simple variable
// 	Given:
// 		vars = {
// 			"var1": "value1",
// 			"var2": "value2",
// 			}
//	Then:
//		"${var1}" = "value1"
//		"X_${var1}_${var2} = "X_value1_value2
//
// Example 2: Array variable
// 	Given:
// 		vars = {
// 			"arr1": "value1,value2,value3",
// 			}
//	Then:
//		"${arr1[0]_arr1[1]_arr1[2]}" = "value1_value2_value3"
//
func ResolveAllVars(args string) ([]string, error) {
	return ResolveAll(vars, strings.Split(args, ","))
}

func newPrivateCollectionConfig(collName string, requiredPeerCount, maxPeerCount int32, blocksToLive uint64, policy *common.SignaturePolicyEnvelope) *common.CollectionConfig {
	return &common.CollectionConfig{
		Payload: &common.CollectionConfig_StaticCollectionConfig{
			StaticCollectionConfig: &common.StaticCollectionConfig{
				Name:              collName,
				RequiredPeerCount: requiredPeerCount,
				MaximumPeerCount:  maxPeerCount,
				BlockToLive:       blocksToLive,
				MemberOrgsPolicy: &common.CollectionPolicyConfig{
					Payload: &common.CollectionPolicyConfig_SignaturePolicy{
						SignaturePolicy: policy,
					},
				},
			},
		},
	}
}

// NewChaincodePolicy parses the policy string and returns the chaincode policy
func NewChaincodePolicy(bddCtx *BDDContext, ccPolicy, channelID string) (*fabricCommon.SignaturePolicyEnvelope, error) {
	if ccPolicy != "" {
		// Create a signature policy from the policy expression passed in
		return newPolicy(ccPolicy)
	}

	netwkConfig := bddCtx.clientConfig.NetworkConfig()

	// Default policy is 'signed by any member' for all known orgs
	var mspIDs []string
	for _, orgID := range bddCtx.OrgsByChannel(channelID) {
		orgConfig, ok := netwkConfig.Organizations[strings.ToLower(orgID)]
		if !ok {
			return nil, errors.Errorf("org config not found for org ID %s", orgID)
		}
		mspIDs = append(mspIDs, orgConfig.MSPID)
	}
	logger.Infof("Returning SignedByAnyMember policy for MSPs %s", mspIDs)
	return cauthdsl.SignedByAnyMember(mspIDs), nil
}

// RegisterSteps register steps
func (d *CommonSteps) RegisterSteps(s *godog.Suite) {
	s.BeforeScenario(d.BDDContext.BeforeScenario)
	s.AfterScenario(d.BDDContext.AfterScenario)

	s.Step(`^the channel "([^"]*)" is created and all peers have joined$`, d.createChannelAndJoinAllPeers)
	s.Step(`^the channel "([^"]*)" is created and all peers from org "([^"]*)" have joined$`, d.createChannelAndJoinPeersFromOrg)
	s.Step(`^we wait (\d+) seconds$`, d.wait)
	s.Step(`^client queries chaincode "([^"]*)" with args "([^"]*)" on all peers in the "([^"]*)" org on the "([^"]*)" channel$`, d.queryCConOrg)
	s.Step(`^client queries chaincode "([^"]*)" with args "([^"]*)" on a single peer in the "([^"]*)" org on the "([^"]*)" channel$`, d.queryCConSinglePeerInOrg)
	s.Step(`^client queries chaincode "([^"]*)" with args "([^"]*)" on peers "([^"]*)" on the "([^"]*)" channel$`, d.queryCConTargetPeers)
	s.Step(`^client queries system chaincode "([^"]*)" with args "([^"]*)" on org "([^"]*)" peer on the "([^"]*)" channel$`, d.querySystemCC)
	s.Step(`^client queries chaincode "([^"]*)" with args "([^"]*)" on the "([^"]*)" channel$`, d.queryCC)
	s.Step(`^client queries chaincode "([^"]*)" with args "([^"]*)" on the "([^"]*)" channel then the error response should contain "([^"]*)"$`, d.queryCCWithError)
	s.Step(`^response from "([^"]*)" to client contains value "([^"]*)"$`, d.containsInQueryValue)
	s.Step(`^response from "([^"]*)" to client equal value "([^"]*)"$`, d.equalQueryValue)
	s.Step(`^"([^"]*)" chaincode "([^"]*)" version "([^"]*)" is installed from path "([^"]*)" to all peers$`, d.installChaincodeToAllPeersWithVersion)
	s.Step(`^"([^"]*)" chaincode "([^"]*)" is installed from path "([^"]*)" to all peers$`, d.installChaincodeToAllPeers)
	s.Step(`^"([^"]*)" chaincode "([^"]*)" is installed from path "([^"]*)" to all peers in the "([^"]*)" org$`, d.installChaincodeToOrg)
	s.Step(`^"([^"]*)" chaincode "([^"]*)" is installed from path "([^"]*)" to all peers except "([^"]*)"$`, d.installChaincodeToAllPeersExcept)
	s.Step(`^"([^"]*)" chaincode "([^"]*)" is instantiated from path "([^"]*)" on all peers in the "([^"]*)" org on the "([^"]*)" channel with args "([^"]*)" with endorsement policy "([^"]*)" with collection policy "([^"]*)"$`, d.instantiateChaincodeOnOrg)
	s.Step(`^"([^"]*)" chaincode "([^"]*)" is instantiated from path "([^"]*)" on the "([^"]*)" channel with args "([^"]*)" with endorsement policy "([^"]*)" with collection policy "([^"]*)"$`, d.instantiateChaincode)
	s.Step(`^"([^"]*)" chaincode "([^"]*)" is upgraded with version "([^"]*)" from path "([^"]*)" on the "([^"]*)" channel with args "([^"]*)" with endorsement policy "([^"]*)" with collection policy "([^"]*)"$`, d.upgradeChaincode)
	s.Step(`^"([^"]*)" chaincode "([^"]*)" is upgraded with version "([^"]*)" from path "([^"]*)" on the "([^"]*)" channel with args "([^"]*)" with endorsement policy "([^"]*)" with collection policy "([^"]*)" then the error response should contain "([^"]*)"$`, d.upgradeChaincodeWithError)
	s.Step(`^"([^"]*)" chaincode "([^"]*)" is deployed from path "([^"]*)" to all peers in the "([^"]*)" org on the "([^"]*)" channel with args "([^"]*)" with endorsement policy "([^"]*)" with collection policy "([^"]*)"$`, d.deployChaincodeToOrg)
	s.Step(`^"([^"]*)" chaincode "([^"]*)" is deployed from path "([^"]*)" to all peers on the "([^"]*)" channel with args "([^"]*)" with endorsement policy "([^"]*)" with collection policy "([^"]*)"$`, d.deployChaincode)
	s.Step(`^chaincode "([^"]*)" is warmed up on all peers in the "([^"]*)" org on the "([^"]*)" channel$`, d.warmUpCConOrg)
	s.Step(`^chaincode "([^"]*)" is warmed up on all peers on the "([^"]*)" channel$`, d.warmUpCC)
	s.Step(`^client invokes chaincode "([^"]*)" with args "([^"]*)" on all peers in the "([^"]*)" org on the "([^"]*)" channel$`, d.InvokeCConOrg)
	s.Step(`^client invokes chaincode "([^"]*)" with args "([^"]*)" on the "([^"]*)" channel$`, d.InvokeCC)
	s.Step(`^client invokes chaincode "([^"]*)" with args "([^"]*)" on peers "([^"]*)" on the "([^"]*)" channel$`, d.invokeCConTargetPeers)
	s.Step(`^collection config "([^"]*)" is defined for collection "([^"]*)" as policy="([^"]*)", requiredPeerCount=(\d+), maxPeerCount=(\d+), and blocksToLive=(\d+)$`, d.defineCollectionConfig)
	s.Step(`^block (\d+) from the "([^"]*)" channel is displayed$`, d.displayBlockFromChannel)
	s.Step(`^the last (\d+) blocks from the "([^"]*)" channel are displayed$`, d.displayBlocksFromChannel)
	s.Step(`^the last block from the "([^"]*)" channel is displayed$`, d.displayLastBlockFromChannel)
	s.Step(`^the response is saved to variable "([^"]*)"$`, d.setVariableFromCCResponse)
	s.Step(`^variable "([^"]*)" is assigned the JSON value '([^']*)'$`, d.setJSONVariable)
	s.Step(`^the JSON path "([^"]*)" of the response equals "([^"]*)"$`, d.jsonPathOfCCResponseEquals)
	s.Step(`^the JSON path "([^"]*)" of the response has (\d+) items$`, d.jsonPathOfCCHasNumItems)
	s.Step(`^the JSON path "([^"]*)" of the response contains "([^"]*)"$`, d.jsonPathOfCCResponseContains)
}
