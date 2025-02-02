package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Rican7/retry"
	"github.com/Rican7/retry/strategy"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/accounts/keystore"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/hashicorp/go-hclog"
	"github.com/meshplus/bitxhub-core/agency"
	"github.com/meshplus/bitxhub-model/pb"
)

//go:generate abigen --sol ./example/broker.sol --pkg main --out broker.go
//go:generate abigen --sol ./example/broker_direct.sol --pkg main --out broker_direct.go
type Client struct {
	abi           abi.ABI
	config        *Config
	ctx           context.Context
	cancel        context.CancelFunc
	ethClient     *ethclient.Client
	session       *BrokerSession
	sessionDirect *BrokerDirectSession
	eventC        chan *pb.IBTP
	reqCh         chan *pb.GetDataRequest
	lock          sync.Mutex
}

var (
	_      agency.Client = (*Client)(nil)
	logger               = hclog.New(&hclog.LoggerOptions{
		Name:   "client",
		Output: os.Stderr,
		Level:  hclog.Trace,
	})
	EtherType = "ethereum"
)

const (
	SubmitIBTPErr    = "SubmitIBTP tx execution failed"
	SubmitReceiptErr = "SubmitReceipt tx execution failed"
)

func (c *Client) GetUpdateMeta() chan *pb.UpdateMeta {
	panic("implement me")
}

func (c *Client) Initialize(configPath string, _ []byte, mode string) error {
	cfg, err := UnmarshalConfig(configPath)
	if err != nil {
		return fmt.Errorf("unmarshal config for plugin :%w", err)
	}

	logger.Info("Basic appchain info",
		"broker address", cfg.Ether.ContractAddress,
		"ethereum node ip", cfg.Ether.Addr)

	etherCli, err := ethclient.Dial(cfg.Ether.Addr)
	if err != nil {
		return fmt.Errorf("dial ethereum node: %w", err)
	}

	keyPath := filepath.Join(configPath, cfg.Ether.KeyPath)
	keyByte, err := ioutil.ReadFile(keyPath)
	if err != nil {
		return err
	}

	psdPath := filepath.Join(configPath, cfg.Ether.Password)
	password, err := ioutil.ReadFile(psdPath)
	if err != nil {
		return err
	}

	unlockedKey, err := keystore.DecryptKey(keyByte, strings.TrimSpace(string(password)))
	if err != nil {
		return err
	}

	chainID, err := etherCli.ChainID(context.TODO())
	if err != nil {
		return fmt.Errorf("cannot get ethereum chain ID: %sv", err)
	}

	// deploy a contract first
	auth, err := bind.NewKeyedTransactorWithChainID(unlockedKey.PrivateKey, chainID)
	if err != nil {
		return err
	}
	if auth.Context == nil {
		auth.Context = context.TODO()
	}
	auth.Value = nil
	if mode == relayMode {
		broker, err := NewBroker(common.HexToAddress(cfg.Ether.ContractAddress), etherCli)
		if err != nil {
			return fmt.Errorf("failed to instantiate a Broker contract: %w", err)
		}
		session := &BrokerSession{
			Contract: broker,
			CallOpts: bind.CallOpts{
				Pending: false,
			},
			TransactOpts: *auth,
		}
		c.session = session
	} else {
		broker, err := NewBrokerDirect(common.HexToAddress(cfg.Ether.ContractAddress), etherCli)
		if err != nil {
			return fmt.Errorf("failed to instantiate a Broker contract: %w", err)
		}
		sessionDirect := &BrokerDirectSession{
			Contract: broker,
			CallOpts: bind.CallOpts{
				Pending: false,
			},
			TransactOpts: *auth,
		}
		c.sessionDirect = sessionDirect
	}

	ab, err := abi.JSON(bytes.NewReader([]byte(BrokerABI)))
	if err != nil {
		return fmt.Errorf("abi unmarshal: %s", err.Error())
	}

	c.config = cfg
	c.eventC = make(chan *pb.IBTP, 1024)
	c.reqCh = make(chan *pb.GetDataRequest, 1024)
	c.ethClient = etherCli
	c.abi = ab
	c.ctx, c.cancel = context.WithCancel(context.Background())
	return nil
}

func (c *Client) Start() error {
	if c.session == nil {
		return c.StartDirectConsumer()
	}
	return c.StartConsumer()
}

func (c *Client) Stop() error {
	c.cancel()
	return nil
}

func (c *Client) GetIBTPCh() chan *pb.IBTP {
	return c.eventC
}

func (c *Client) Name() string {
	return c.config.Ether.Name
}

func (c *Client) Type() string {
	return EtherType
}

// SubmitIBTP submit interchain ibtp. It will unwrap the ibtp and execute
// the function inside the ibtp. If any execution results returned, pass
// them to other modules.
func (c *Client) SubmitIBTP(from string, index uint64, serviceID string, ibtpType pb.IBTP_Type, content *pb.Content, proof *pb.BxhProof, isEncrypted bool) (*pb.SubmitIBTPResponse, error) {
	// check offChain contract addr
	if strings.EqualFold(serviceID, c.config.Ether.OffChainAddr) {
		if needOffChain := CheckInterchainOffChain(content); needOffChain {
			bxhID, chainID, err := c.GetChainID()
			if err != nil {
				logger.Warn("call GetChainID failed", "error", err.Error())
				return nil, err
			}

			// get data from srcChain
			req := constructReq(index, fmt.Sprintf("%s:%s:%s", bxhID, chainID, serviceID), from, content.Args[1])
			c.reqCh <- req
		}
	}

	ret := &pb.SubmitIBTPResponse{Status: true}
	//if 0 != strings.Compare(common.HexToAddress(serviceID).Hex(), serviceID) {
	//	logger.Warn("destAddr checkSum failed",
	//		"destAddr", serviceID,
	//		"destCheckSumAddr", common.HexToAddress(serviceID).Hex(),
	//	)
	//	ret.Status = false
	//	return ret, nil
	//}
	typ := int64(binary.BigEndian.Uint64(content.Args[0]))
	if typ == int64(pb.IBTP_Multi) {
		lenArgs := len(content.Args) - 2
		num := int(binary.BigEndian.Uint64(content.Args[1])) //convert byte to Uint64
		if lenArgs%num != 0 {
			return ret, fmt.Errorf("format error for IBTP carrying multiple transactions")
		}

		var Args [][][]byte
		for i := 2; i < len(content.Args); {
			Args = append(Args, content.Args[i:i+num])
			i += num
		}
		receipt, err := c.InvokeMultiInterchain(from, index, serviceID, uint64(ibtpType), content.Func, Args, uint64(proof.TxStatus), proof.MultiSign, isEncrypted)
		if err != nil {
			ret.Status = false
			ret.Message = err.Error()
			logger.Warn("SubmitIBTP:", ret.Status, ret.Message)
			return ret, nil
		}
		if receipt.Status != types.ReceiptStatusSuccessful {
			ret.Status = false
			ret.Message = SubmitIBTPErr
			return ret, nil
		}
		logger.Info("SubmitIBTP:", ret.Status, ret.Message, "txHash: ", receipt.TxHash)
	} else {
		content.Args = content.Args[1:]
		receipt, err := c.invokeInterchain(from, index, serviceID, uint64(ibtpType), content.Func, content.Args, uint64(proof.TxStatus), proof.MultiSign, isEncrypted)
		if err != nil {
			ret.Status = false
			ret.Message = err.Error()
			logger.Warn("SubmitIBTP:", ret.Status, ret.Message)
			return ret, nil
		}

		if receipt.Status != types.ReceiptStatusSuccessful {
			ret.Status = false
			ret.Message = SubmitIBTPErr
			return ret, nil
		}
		logger.Info("SubmitIBTP:", ret.Status, ret.Message, "txHash: ", receipt.TxHash)
	}

	return ret, nil
}

func (c *Client) SubmitReceipt(to string, index uint64, serviceID string, ibtpType pb.IBTP_Type, result *pb.Result, proof *pb.BxhProof) (*pb.SubmitIBTPResponse, error) {
	if strings.EqualFold(serviceID, c.config.Ether.OffChainAddr) {
		bxhID, chainID, err := c.GetChainID()
		if err != nil {
			logger.Warn("call GetChainID failed", "error", err.Error())
			return nil, err
		}
		from := fmt.Sprintf("%s:%s:%s", bxhID, chainID, serviceID)
		ibtp, err := c.GetOutMessage(fmt.Sprintf("%s-%s", from, to), index)
		if err != nil {
			logger.Warn("call GetOutMessage failed", "error", err.Error())
			return nil, err
		}
		needOffChain, err := CheckReceiptOffChain(ibtp, result)
		if err != nil {
			logger.Warn("check offChain flag", "error", err.Error())
			return nil, err
		}
		if needOffChain {
			// get data from dstChain
			var results [][][]byte
			for _, s := range result.Data {
				results = append(results, s.Data)
			}
			req := constructReq(index, from, to, results[0][0])
			c.reqCh <- req
		}
	}

	ret := &pb.SubmitIBTPResponse{Status: true}
	var results [][][]byte
	for _, s := range result.Data {
		results = append(results, s.Data)
	}

	// if src chain need rollback, the length of results is 0
	if len(result.MultiStatus) > 1 || (len(result.MultiStatus) == 0 && proof.TxStatus != pb.TransactionStatus_BEGIN) {
		receipt, err := c.InvokeMultiReceipt(serviceID, to, index, uint64(ibtpType), results, result.MultiStatus, uint64(proof.TxStatus), proof.MultiSign)
		if err != nil {
			ret.Status = false
			ret.Message = err.Error()
			return ret, nil
		}

		if receipt.Status != types.ReceiptStatusSuccessful {
			ret.Status = false
			ret.Message = SubmitReceiptErr
		}

	} else {
		// The case where a rollback is required in the source chain of a single transaction
		receipt, err := c.invokeReceipt(serviceID, to, index, uint64(ibtpType), results, uint64(proof.TxStatus), proof.MultiSign)
		if err != nil {
			ret.Status = false
			ret.Message = err.Error()
			return ret, nil
		}

		if receipt.Status != types.ReceiptStatusSuccessful {
			ret.Status = false
			ret.Message = SubmitReceiptErr
		}
	}

	return ret, nil
}

func (c *Client) SubmitIBTPBatch(from []string, index []uint64, serviceID []string, ibtpType []pb.IBTP_Type, content []*pb.Content, proof []*pb.BxhProof, isEncrypted []bool) (*pb.SubmitIBTPResponse, error) {
	ret := &pb.SubmitIBTPResponse{Status: true}
	var (
		callFunc []string
		args     [][][]byte
		typ      []uint64
		txStatus []uint64
		sign     [][][]byte
		tx       *types.Transaction
		txErr    error
	)
	for idx, ct := range content {
		callFunc = append(callFunc, ct.Func)
		ct.Args = ct.Args[1:]
		args = append(args, ct.Args)
		typ = append(typ, uint64(ibtpType[idx]))
		txStatus = append(txStatus, uint64(proof[idx].TxStatus))
		sign = append(sign, proof[idx].MultiSign)
	}

	if err := retry.Retry(func(attempt uint) error {
		tx, txErr = c.session.InvokeInterchains(from, serviceID, index, typ, callFunc, args, txStatus, sign, isEncrypted)
		if txErr != nil {
			if strings.Contains(txErr.Error(), "execution reverted") {
				return nil
			}
		}

		return txErr
	}, strategy.Wait(2*time.Second)); err != nil {
		logger.Error("Can't invoke contract", "error", err)
	}
	if txErr != nil {
		ret.Status = false
		ret.Message = txErr.Error()
		return ret, nil
	}

	receipt := c.waitForConfirmed(tx.Hash())

	if receipt.Status != types.ReceiptStatusSuccessful {
		ret.Status = false
		ret.Message = SubmitIBTPErr
		return ret, nil
	}

	return ret, nil
}

func (c *Client) SubmitReceiptBatch(_ []string, _ []uint64, _ []string, _ []pb.IBTP_Type, _ []*pb.Result, _ []*pb.BxhProof) (*pb.SubmitIBTPResponse, error) {
	panic("implement me")
}

//nolint:dupl
func (c *Client) invokeInterchain(srcFullID string, index uint64, destAddr string, reqType uint64, callFunc string, args [][]byte, txStatus uint64, multiSign [][]byte, encrypt bool) (*types.Receipt, error) {
	c.lock.Lock()
	var tx *types.Transaction
	var txErr error
	if err := retry.Retry(func(attempt uint) error {
		if c.session == nil {
			tx, txErr = c.sessionDirect.InvokeInterchain(srcFullID, destAddr, index, reqType, callFunc, args, txStatus, multiSign, encrypt)
		} else {
			tx, txErr = c.session.InvokeInterchain(srcFullID, destAddr, index, reqType, callFunc, args, txStatus, multiSign, encrypt)
		}
		if txErr != nil {
			logger.Warn("Call InvokeInterchain failed",
				"srcFullID", srcFullID,
				"destAddr", destAddr,
				"index", fmt.Sprintf("%d", index),
				"reqType", strconv.Itoa(int(reqType)),
				"callFunc", callFunc,
				"args", string(bytes.Join(args, []byte(","))),
				"txStatus", strconv.Itoa(int(txStatus)),
				"multiSign size", strconv.Itoa(len(multiSign)),
				"encrypt", strconv.FormatBool(encrypt),
				"error", txErr.Error(),
			)

			for i, arg := range args {
				logger.Warn("args", strconv.Itoa(i), hexutil.Encode(arg))
			}

			for i, sign := range multiSign {
				logger.Warn("multiSign", strconv.Itoa(i), hexutil.Encode(sign))
			}

			if strings.Contains(txErr.Error(), "execution reverted") {
				return nil
			}
		}

		return txErr
	}, strategy.Wait(2*time.Second)); err != nil {
		logger.Error("Can't invoke contract", "error", err)
	}
	c.lock.Unlock()

	if txErr != nil {
		return nil, txErr
	}
	return c.waitForConfirmed(tx.Hash()), nil
}

//nolint:dupl
func (c *Client) InvokeMultiInterchain(srcFullID string, index uint64, destAddr string, reqType uint64, callFunc string, args [][][]byte, txStatus uint64, multiSign [][]byte, encrypt bool) (*types.Receipt, error) {
	arg := make([][]byte, len(args))
	for i := 0; i < len(args); i++ {
		arg[i] = bytes.Join(args[i], []byte(","))
	}
	c.lock.Lock()
	var tx *types.Transaction
	var txErr error
	if err := retry.Retry(func(attempt uint) error {
		if c.session == nil {
			tx, txErr = c.sessionDirect.InvokeMultiInterchain(srcFullID, destAddr, index, reqType, callFunc, args, txStatus, multiSign, encrypt)
		} else {
			tx, txErr = c.session.InvokeMultiInterchain(srcFullID, destAddr, index, reqType, callFunc, args, txStatus, multiSign, encrypt)
		}
		if txErr != nil {
			logger.Warn("Call InvokeMultiInterchain failed",
				"srcFullID", srcFullID,
				"destAddr", destAddr,
				"index", fmt.Sprintf("%d", index),
				"reqType", strconv.Itoa(int(reqType)),
				"callFunc", callFunc,
				"args", string(bytes.Join(arg, []byte(","))),
				"txStatus", strconv.Itoa(int(txStatus)),
				"multiSign size", strconv.Itoa(len(multiSign)),
				"encrypt", strconv.FormatBool(encrypt),
				"error", txErr.Error(),
			)

			for i, Arg := range arg {
				logger.Warn("args", strconv.Itoa(i), hexutil.Encode(Arg))
			}

			for i, sign := range multiSign {
				logger.Warn("multiSign", strconv.Itoa(i), hexutil.Encode(sign))
			}

			if strings.Contains(txErr.Error(), "execution reverted") {
				return nil
			}
		}

		return txErr
	}, strategy.Wait(2*time.Second)); err != nil {
		logger.Error("Can't invoke contract", "error", err)
	}
	c.lock.Unlock()

	if txErr != nil {
		return nil, txErr
	}
	return c.waitForConfirmed(tx.Hash()), nil
}

func (c *Client) invokeReceipt(srcAddr string, dstFullID string, index uint64, reqType uint64, results [][][]byte, txStatus uint64, multiSign [][]byte) (*types.Receipt, error) {
	result := make([][]byte, len(results))
	for i := 0; i < len(results); i++ {
		result[i] = bytes.Join(results[i], []byte(","))
	}
	c.lock.Lock()
	var tx *types.Transaction
	var txErr error
	if err := retry.Retry(func(attempt uint) error {
		if c.session == nil {
			tx, txErr = c.sessionDirect.InvokeReceipt(srcAddr, dstFullID, index, reqType, results, txStatus, multiSign)
		} else {
			tx, txErr = c.session.InvokeReceipt(srcAddr, dstFullID, index, reqType, results, txStatus, multiSign)
		}
		if txErr != nil {
			logger.Warn("Call InvokeReceipt failed",
				"srcAddr", srcAddr,
				"dstFullID", dstFullID,
				"index", fmt.Sprintf("%d", index),
				"reqType", strconv.Itoa(int(reqType)),
				"result", string(bytes.Join(result, []byte(","))),
				"txStatus", strconv.Itoa(int(txStatus)),
				"multiSign size", strconv.Itoa(len(multiSign)),
				"error", txErr.Error(),
			)

			for i, arg := range result {
				logger.Warn("result", strconv.Itoa(i), hexutil.Encode(arg))
			}

			for i, sign := range multiSign {
				logger.Warn("multiSign", strconv.Itoa(i), hexutil.Encode(sign))
			}

			if strings.Contains(txErr.Error(), "execution reverted") {
				return nil
			}
		}

		return txErr
	}, strategy.Wait(2*time.Second)); err != nil {
		logger.Error("Can't invoke contract", "error", err)
	}
	c.lock.Unlock()
	if txErr != nil {
		return nil, txErr
	}

	return c.waitForConfirmed(tx.Hash()), nil
}

func (c *Client) InvokeMultiReceipt(srcAddr string, destFullID string, index uint64, reqType uint64, results [][][]byte, multiStatus []bool, txStatus uint64, multiSign [][]byte) (*types.Receipt, error) {
	result := make([][]byte, len(results))
	for i := 0; i < len(results); i++ {
		result[i] = bytes.Join(results[i], []byte(","))
	}
	c.lock.Lock()
	var tx *types.Transaction
	var txErr error
	if err := retry.Retry(func(attempt uint) error {
		if c.session == nil {
			tx, txErr = c.sessionDirect.InvokeMultiReceipt(srcAddr, destFullID, index, reqType, results, multiStatus, txStatus, multiSign)
		} else {
			tx, txErr = c.session.InvokeMultiReceipt(srcAddr, destFullID, index, reqType, results, multiStatus, txStatus, multiSign)
		}
		if txErr != nil {
			logger.Warn("Call InvokeReceipt failed",
				"srcAddr", srcAddr,
				"dstFullID", destFullID,
				"index", fmt.Sprintf("%d", index),
				"reqType", strconv.Itoa(int(reqType)),
				"result", string(bytes.Join(result, []byte(","))),
				"txStatus", strconv.Itoa(int(txStatus)),
				"multiSign size", strconv.Itoa(len(multiSign)),
				"error", txErr.Error(),
			)

			for i, arg := range result {
				logger.Warn("result", strconv.Itoa(i), hexutil.Encode(arg))
			}

			for i, sign := range multiSign {
				logger.Warn("multiSign", strconv.Itoa(i), hexutil.Encode(sign))
			}

			if strings.Contains(txErr.Error(), "execution reverted") {
				return nil
			}
		}

		return txErr
	}, strategy.Wait(2*time.Second)); err != nil {
		logger.Error("Can't invoke contract", "error", err)
	}
	c.lock.Unlock()
	if txErr != nil {
		return nil, txErr
	}

	return c.waitForConfirmed(tx.Hash()), nil
}

// GetOutMessage gets crosschain tx by `to` address and index
func (c *Client) GetOutMessage(servicePair string, idx uint64) (*pb.IBTP, error) {
	srcService, dstService, err := pb.ParseServicePair(servicePair)
	if err != nil {
		return nil, err
	}

	if c.session == nil {
		ev := &BrokerDirectThrowInterchainEvent{
			Index:     idx,
			DstFullID: dstService,
			SrcFullID: srcService,
		}

		return c.Convert2DirectIBTP(ev, int64(c.config.Ether.TimeoutHeight))
	} else {
		ev := &BrokerThrowInterchainEvent{
			Index:     idx,
			DstFullID: dstService,
			SrcFullID: srcService,
		}

		return c.Convert2IBTP(ev, int64(c.config.Ether.TimeoutHeight))
	}
}

// GetReceiptMessage gets the execution results from contract by from-index key
func (c *Client) GetReceiptMessage(servicePair string, idx uint64) (*pb.IBTP, error) {
	var (
		data        [][][]byte
		typ         uint64
		encrypt     bool
		multiStatus []bool
	)

	if err := retry.Retry(func(attempt uint) error {
		var err error
		if c.session == nil {
			data, typ, encrypt, multiStatus, err = c.sessionDirect.GetReceiptMessage(servicePair, idx)
		} else {
			data, typ, encrypt, multiStatus, err = c.session.GetReceiptMessage(servicePair, idx)
		}
		if err != nil {
			logger.Error("get receipt message", "servicePair", servicePair, "err", err.Error())
		}
		return err
	}); err != nil {
		logger.Error("retry error in GetInMessage", "err", err.Error())
		return nil, err
	}

	srcServiceID, dstServiceID, err := pb.ParseServicePair(servicePair)
	if err != nil {
		return nil, err
	}

	return generateReceipt(srcServiceID, dstServiceID, idx, data, typ, encrypt, multiStatus)
}

// GetInMeta queries contract about how many interchain txs have been
// executed on this appchain for different source chains.
func (c *Client) GetInMeta() (map[string]uint64, error) {
	if c.session == nil {
		return c.getMeta(c.sessionDirect.GetInnerMeta)
	}
	return c.getMeta(c.session.GetInnerMeta)
}

// GetOutMeta queries contract about how many interchain txs have been
// sent out on this appchain to different destination chains.
func (c *Client) GetOutMeta() (map[string]uint64, error) {
	if c.session == nil {
		return c.getMeta(c.sessionDirect.GetOuterMeta)
	}
	return c.getMeta(c.session.GetOuterMeta)
}

// GetCallbackMeta queries contract about how many callback functions have been
// executed on this appchain from different destination chains.
func (c *Client) GetCallbackMeta() (map[string]uint64, error) {
	if c.session == nil {
		return c.getMeta(c.sessionDirect.GetCallbackMeta)
	}
	return c.getMeta(c.session.GetCallbackMeta)
}

func (c *Client) getMeta(getMetaFunc func() ([]string, []uint64, error)) (map[string]uint64, error) {
	var (
		appchainIDs []string
		indices     []uint64
		err         error
	)
	meta := make(map[string]uint64, 0)

	appchainIDs, indices, err = getMetaFunc()
	if err != nil {
		return nil, err
	}

	for i, did := range appchainIDs {
		meta[did] = indices[i]
	}

	return meta, nil
}

func (c *Client) getBestBlock() uint64 {
	var blockNum uint64

	if err := retry.Retry(func(attempt uint) error {
		var err error
		blockNum, err = c.ethClient.BlockNumber(c.ctx)
		if err != nil {
			logger.Error("retry failed in getting best block", "err", err.Error())
		}
		return err
	}, strategy.Wait(time.Second*10)); err != nil {
		logger.Error("retry failed in get best block", "err", err.Error())
		panic(err)
	}

	return blockNum
}

func (c *Client) waitForConfirmed(hash common.Hash) *types.Receipt {
	var (
		receipt *types.Receipt
		err     error
	)

	start := c.getBestBlock()

	for c.getBestBlock()-start < c.config.Ether.MinConfirm {
		time.Sleep(time.Second * 5)
	}
	if err := retry.Retry(func(attempt uint) error {
		receipt, err = c.ethClient.TransactionReceipt(c.ctx, hash)
		if err != nil {
			return err
		}

		return nil
	}, strategy.Wait(2*time.Second)); err != nil {
		logger.Error("Can't get receipt for tx", hash.Hex(), "error", err)
	}

	return receipt
}

func (c *Client) GetDstRollbackMeta() (map[string]uint64, error) {
	if c.session == nil {
		return c.getMeta(c.sessionDirect.GetDstRollbackMeta)
	}
	return c.getMeta(c.session.GetDstRollbackMeta)
}

func (c *Client) GetDirectTransactionMeta(IBTPid string) (uint64, uint64, uint64, error) {
	timestamp, txStatus, err := c.sessionDirect.GetDirectTransactionMeta(IBTPid)
	if err != nil {
		return 0, 0, 0, err
	}

	return timestamp.Uint64(), c.config.Ether.TimeoutPeriod, txStatus, nil
}

func (c *Client) GetChainID() (string, string, error) {
	if c.session == nil {
		return c.sessionDirect.GetChainID()
	}
	return c.session.GetChainID()
}

func (c *Client) GetServices() ([]string, error) {
	if c.session == nil {
		return c.sessionDirect.GetLocalServiceList()
	}
	return c.session.GetLocalServiceList()
}

func (c *Client) GetAppchainInfo(chainID string) (string, []byte, string, error) {
	broker, trustRoot, ruleAddr, err := c.sessionDirect.GetAppchainInfo(chainID)
	if err != nil {
		return "", nil, "", err
	}

	return broker, trustRoot, ruleAddr.String(), nil
}

func (c *Client) GetOffChainData(request *pb.GetDataRequest) (*pb.OffChainDataInfo, error) {
	fi, err := os.Stat(string(request.Req))
	if err != nil {
		return nil, fmt.Errorf("get file stat failed: %w", err)
	}

	return &pb.OffChainDataInfo{
		Filename: fi.Name(),
		Filesize: fi.Size(),
		Filepath: string(request.Req),
	}, nil
}

func (c *Client) GetOffChainDataReq() chan *pb.GetDataRequest {
	return c.reqCh
}

func (c *Client) SubmitOffChainData(response *pb.GetDataResponse) error {
	if response.Type == pb.GetDataResponse_DATA_GET_SUCCESS {
		//// download offChain data
		//path := filepath.Join(string(response.Data), response.Msg)
		//data, err := ioutil.ReadFile(path)
		//if err != nil {
		//	return fmt.Errorf("download offChain data with path(%s): %w", path, err)
		//}
		//
		//// save offChain data
		//if err := ioutil.WriteFile(filepath.Join(c.config.Ether.OffChainPath, response.Msg), data, 0644); err != nil {
		//	return fmt.Errorf("save offChain data: %w", err)
		//}
		//return nil
		name := response.Msg + "-" + time.Now().Format("2006.01.02-15:04:05")
		savePath := filepath.Join(c.config.Ether.OffChainPath, name)
		mf, err := os.OpenFile(savePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, os.ModePerm)
		if err != nil {
			return err
		}
		defer mf.Close()

		var sf *os.File
		defer sf.Close()
		for i := uint64(1); i <= response.ShardTag.ShardSize; i++ {
			name := fmt.Sprintf("%s-%s-%d-%d-%d", response.From, response.To, response.Index, i, response.ShardTag.ShardSize)
			path := filepath.Join(string(response.Data), name)
			sf, err = os.Open(path)
			if err != nil {
				return err
			}
			data, err := ioutil.ReadAll(sf)
			if err != nil {
				return err
			}
			_, err = mf.Write(data)
			if err != nil {
				return err
			}
		}
		return nil
	}

	return fmt.Errorf("%s:%s", response.Type.String(), response.Msg)
}
