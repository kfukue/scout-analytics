package main

// Event topics (topic0 of a log), all in this one file: every event topic the
// package uses is declared here and nowhere else, so a change to a topic
// reaches every place that uses it without searching the code. Never declare
// a topic in the file that uses it. Tests may write the expected hash as a
// literal (that checks the signature).
//
// Each topic has a one-line comment with the event's full signature;
// "indexed" marks a parameter that is a log topic, where the code or its
// comments say so. topicOf (onchain.go) hashes the signature.

var (
	// ---- ERC-20 ----

	// Transfer(address indexed from, address indexed to, uint256 value)
	topicTransfer = topicOf("Transfer(address,address,uint256)")

	// ---- Uniswap v2 (pair) ----

	// Sync(uint112 reserve0, uint112 reserve1)
	topicSyncV2 = topicOf("Sync(uint112,uint112)")

	// Mint(address indexed sender, uint256 amount0, uint256 amount1)
	topicMintV2 = topicOf("Mint(address,uint256,uint256)")

	// Burn(address indexed sender, uint256 amount0, uint256 amount1, address indexed to)
	topicBurnV2 = topicOf("Burn(address,uint256,uint256,address)")

	// ---- Uniswap v3 (pool) ----

	// Swap(address, address, int256 amount0, int256 amount1, uint160 sqrtPriceX96, uint128 liquidity, int24 tick)
	topicSwapV3 = topicOf("Swap(address,address,int256,int256,uint160,uint128,int24)")

	// Mint(address sender, address indexed owner, int24 indexed tickLower, int24 indexed tickUpper, uint128 amount, uint256 amount0, uint256 amount1)
	topicMintV3 = topicOf("Mint(address,address,int24,int24,uint128,uint256,uint256)")

	// Burn(address indexed owner, int24 indexed tickLower, int24 indexed tickUpper, uint128 amount, uint256 amount0, uint256 amount1)
	topicBurnV3 = topicOf("Burn(address,int24,int24,uint128,uint256,uint256)")

	// Collect(address indexed owner, address recipient, int24 indexed tickLower, int24 indexed tickUpper, uint128 amount0, uint128 amount1)
	topicCollectV3 = topicOf("Collect(address,address,int24,int24,uint128,uint128)")

	// ---- Uniswap v4 (PoolManager) ----

	// Swap(bytes32 indexed id, address, int128 amount0, int128 amount1, uint160 sqrtPriceX96, uint128 liquidity, int24 tick, uint24)
	topicSwapV4 = topicOf("Swap(bytes32,address,int128,int128,uint160,uint128,int24,uint24)")

	// Initialize(bytes32 indexed id, address indexed currency0, address indexed currency1, uint24 fee, int24 tickSpacing, address hooks, uint160 sqrtPriceX96, int24 tick)
	topicInitV4 = topicOf("Initialize(bytes32,address,address,uint24,int24,address,uint160,int24)")

	// ModifyLiquidity(bytes32 indexed id, address indexed sender, int24 tickLower, int24 tickUpper, int256 liquidityDelta, bytes32 salt)
	topicModifyLiquidityV4 = topicOf("ModifyLiquidity(bytes32,address,int24,int24,int256,bytes32)")

	// ---- Pons V2 (bonding curve and launch factory) ----

	// CurveBuy(address indexed buyer, address indexed recipient, uint256 quoteIn, uint256 tokensOut, uint256 fee, uint256 tax)
	topicCurveBuy = topicOf("CurveBuy(address,address,uint256,uint256,uint256,uint256)")

	// CurveSell(address indexed seller, address indexed recipient, uint256 tokensIn, uint256 quoteOut, uint256 fee, uint256 tax)
	topicCurveSell = topicOf("CurveSell(address,address,uint256,uint256,uint256,uint256)")

	// CurveCompleted(address, uint256, uint256)
	topicCurveCompleted = topicOf("CurveCompleted(address,uint256,uint256)")

	// TokenLaunched(address, address, address, address, uint256, uint256)
	topicTokenLaunched = topicOf("TokenLaunched(address,address,address,address,uint256,uint256)")

	// PoolGraduated(address indexed token, uint256, uint256, uint256)
	topicPoolGraduated = topicOf("PoolGraduated(address,uint256,uint256,uint256)")

	// LaunchSwept(address, uint256, uint256)
	topicLaunchSwept = topicOf("LaunchSwept(address,uint256,uint256)")

	// ---- Chainlink ----

	// Chainlink: proxies don't emit events; the underlying aggregator(s) do.
	// AnswerUpdated(int256 indexed, uint256, uint256)
	topicAnswerUpdated = topicOf("AnswerUpdated(int256,uint256,uint256)")

	// topicAggregatorConfirmed: AggregatorConfirmed(address indexed previous,
	// address indexed latest), emitted by AggregatorProxy v0.7+ when it switches
	// to a new aggregator. The v0.6 proxies (EACAggregatorProxy, most Ethereum
	// feeds) emit nothing when they switch.
	topicAggregatorConfirmed = topicOf("AggregatorConfirmed(address,address)")
)
