package transaction

import (
	"context"
	"crypto/ecdsa"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/zyjblockchain/sandy_log/log"
	"github.com/zyjblockchain/tt_tac/utils"
	"math/big"
	"sync"
)

var stableNonceMap map[string]uint64
var latestNonceMap map[string]uint64
var failNonceMap map[string]uint64 // 使用nonce发送交易失败的nonce

func init() {
	// 初始化stableNonceMap 和 latestNonceMap
	once := &sync.Once{}
	once.Do(func() {
		stableNonceMap = make(map[string]uint64)
		latestNonceMap = make(map[string]uint64)
		failNonceMap = make(map[string]uint64)
	})
}

type ChainClient struct {
	Client  *ethclient.Client
	ChainId *big.Int
	mutex   sync.Mutex
}

// tt链上的rpc接口和eth是通用的
func NewChainClient(chainNetUrl string, chainId *big.Int) *ChainClient {
	// 连接网络
	rpcDial, err := rpc.Dial(chainNetUrl)
	if err != nil {
		return nil
	}
	return &ChainClient{
		Client:  ethclient.NewClient(rpcDial),
		ChainId: chainId,
		mutex:   sync.Mutex{},
	}
}

//  SetFailNonce 设置使用发送交易失败的nonce
func (c *ChainClient) SetFailNonce(address string, nonce uint64) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	failNonceMap[address] = nonce
}

// GetLatestNonce
func (c *ChainClient) GetLatestNonce(address string) (uint64, error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	// 查看是否有之前使用失败的nonce可以再次使用
	failNonce, ok := failNonceMap[address]
	if ok {
		// 使用该nonce
		// 先把此地址的fail nonce记录删除掉
		delete(failNonceMap, address)
		return failNonce, nil
	}

	txNonce, err := c.GetNonce(common.HexToAddress(address))
	if err != nil {
		log.Errorf("从链上获取nonce失败：%v", err)
		return 0, err
	}
	stable, ok := stableNonceMap[address]
	if ok { // 存在
		if txNonce == stable {
			latest := latestNonceMap[address]
			txNonce = latest + 1
			latestNonceMap[address] = txNonce
		} else {
			// 最新的链上nonce已经大于stable
			stableNonceMap[address] = txNonce
			// txNonce和latestNonce比较
			latest := latestNonceMap[address]
			if txNonce <= latest {
				txNonce = latest + 1
			}
			// 更新latestNonce
			latestNonceMap[address] = txNonce
		}
	} else {
		// 记录新地址
		stableNonceMap[address] = txNonce
		latestNonceMap[address] = txNonce
	}
	return txNonce, nil
}

func (c *ChainClient) GetNonce(address common.Address) (uint64, error) {
	return c.Client.NonceAt(context.Background(), address, nil)
}

func (c *ChainClient) SuggestGasPrice() (*big.Int, error) {
	return c.Client.SuggestGasPrice(context.Background())
}

func (c *ChainClient) Close() {
	c.Client.Close()
}

// newTokenRawTx 返回的是rawTransaction
func newTokenRawTx(senderNonce uint64, receiver common.Address, contractAddr common.Address, gasLimit uint64, gasPrice *big.Int, tokenAmount *big.Int) *types.Transaction {
	/**
	transferFun := "0xa9059cbb"
	receiverAddrCode := 000000000000000000000000b1e15fdbe88b7e7c47552e2d33cd5a9b2e0fd478 // eg: 代币接收地址code
	tokenAmountCode := "0000000000000000000000000000000000000000000000000000000000000064" // eg: 转币数量100
	*/
	funcName := "transfer(address,uint256)"
	funcCode := getContractFunctionCode(funcName)
	receiverAddrCode := formatArgs(receiver.Hex())
	AmountCode := formatArgs(tokenAmount.Text(16))

	// 组合生成执行合约的input
	inputData := make([]byte, 0)
	inputData = append(append(funcCode, receiverAddrCode...), AmountCode...) // 顺序千万不能乱，可以在etherscan上找个合约交易查看input data

	// 组装以太坊交易
	return types.NewTransaction(senderNonce, contractAddr, big.NewInt(0), gasLimit, gasPrice, inputData)
}

// signRawTx 对交易进行签名
func signRawTx(rawTx *types.Transaction, chainID *big.Int, prv *ecdsa.PrivateKey) (*types.Transaction, error) {
	signer := types.NewEIP155Signer(chainID)
	signedTx, err := types.SignTx(rawTx, signer, prv)
	return signedTx, err
}

// GetTokenBalance
func (c *ChainClient) GetTokenBalance(address, tokenAddress common.Address) (*big.Int, error) {
	funcName := "balanceOf(address)"
	funcCode := getContractFunctionCode(funcName)

	// 组合生成执行合约的input
	inputData := make([]byte, 0)
	inputData = append(funcCode, formatArgs(address.Hex())...)

	callMsg := ethereum.CallMsg{
		From: address,       // 钱包地址
		To:   &tokenAddress, // 代币合约地址
		Data: inputData,
	}
	result, err := c.Client.CallContract(context.Background(), callMsg, nil)
	if err != nil {
		return nil, err
	}
	res := utils.FormatHex(hexutil.Encode(result))
	if len(res) == 2 {
		return big.NewInt(0), nil
	} else {
		return hexutil.DecodeBig(res)
	}
}

// EstimateTokenTxGas 预估代币转账交易gas used使用量
func (c *ChainClient) EstimateTokenTxGas(tokenAmount *big.Int, from, tokenAddress, receiver common.Address) (uint64, error) {
	funcName := "transfer(address,uint256)"
	funcCode := getContractFunctionCode(funcName)
	receiverAddrCode := formatArgs(receiver.Hex())
	AmountCode := formatArgs(tokenAmount.Text(16))
	// 组合生成执行合约的input
	inputData := make([]byte, 0)
	inputData = append(append(funcCode, receiverAddrCode...), AmountCode...)

	callMsg := ethereum.CallMsg{
		From:     from,
		To:       &tokenAddress,
		GasPrice: nil,
		Data:     inputData,
	}
	return c.Client.EstimateGas(context.Background(), callMsg)
}

// SendTokenTx 发送token交易
func (c *ChainClient) SendTokenTx(private string, nonce, gasLimit uint64, gasPrice *big.Int, receiver, tokenAddress common.Address, tokenAmount *big.Int) (*types.Transaction, error) {
	signedTx, err := c.NewSignedTokenTx(private, nonce, gasLimit, gasPrice, receiver, tokenAddress, tokenAmount)
	if err != nil {
		log.Errorf("生成签名交易失败：error: %v", err)
		return nil, err
	}
	// 把签好名的交易发送到网络
	err = c.Client.SendTransaction(context.Background(), signedTx)
	return signedTx, err
}

// NewSignedTokenTx 新建一个签名交易
func (c *ChainClient) NewSignedTokenTx(private string, nonce, gasLimit uint64, gasPrice *big.Int, receiver, tokenAddress common.Address, tokenAmount *big.Int) (*types.Transaction, error) {
	rawTx := newTokenRawTx(nonce, receiver, tokenAddress, gasLimit, gasPrice, tokenAmount)
	// 对原生交易进行签名
	prv, err := crypto.ToECDSA(common.FromHex(private))
	if err != nil {
		panic(err)
	}
	signedTx, err := signRawTx(rawTx, c.ChainId, prv)
	return signedTx, err
}

// 以太坊token交易
// getContractFunctionCode 计算合约函数code
func getContractFunctionCode(funcName string) []byte {
	h := crypto.Keccak256Hash([]byte(funcName))
	return h.Bytes()[:4]
}

// formatArgs 把参数转换成[32]byte的数组类型
func formatArgs(args string) []byte {
	b := common.FromHex(args)
	var h [32]byte
	if len(b) > len(h) {
		b = b[len(b)-32:]
	}
	copy(h[32-len(b):], b)
	return h[:]
}
