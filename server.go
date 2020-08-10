package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/dgrijalva/jwt-go"
	"github.com/filecoin-project/go-address"
	// "github.com/filecoin-project/lotus/build"
	"github.com/filecoin-project/lotus/chain/types"
	big "github.com/filecoin-project/specs-actors/actors/abi/big"
	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"
)

func main() {
	fmt.Println("Lotus node:", env.LotusAPIDialAddr)

	addrs, err := lotusListMiners()
	if err != nil {
		panic(err)
	}

	fmt.Println("Miners:")
	for _, addr := range addrs {
		fmt.Println("  -", addr.String())
	}

	router := gin.Default()
	router.Use(cors.New(cors.Config{
		AllowOrigins:     []string{"*"},
		AllowMethods:     []string{"POST"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Authorization"},
		ExposeHeaders:    []string{"Content-Length"},
		AllowCredentials: true,
		MaxAge:           12 * time.Hour,
	}))

	router.POST("/oauth/:provider", serveOauth)
	router.POST("/verify", serveVerifyAccount)
	router.GET("/verifiers", serveListVerifiers)
	router.GET("/verified-clients", serveListVerifiedClients)
	router.GET("/account-remaining-bytes/:target_addr", serveCheckAccountRemainingBytes)
	router.GET("/verifier-remaining-bytes/:target_addr", serveCheckVerifierRemainingBytes)
	router.POST("/faucet/:target_addr", serveFaucet)

	router.Run(":" + env.Port)
}

var (
	ErrUnsupportedProvider  = errors.New("unsupported oauth provider")
	ErrUserTooNew           = errors.New("user account is too new")
	ErrSufficientAllowance  = errors.New("allowance is already sufficient")
	ErrAllocatedTooRecently = errors.New("you must wait 30 days in between reallocations")
)

type UserLock string

var (
	UserLock_Verifier UserLock = "Verifier"
	UserLock_Faucet   UserLock = "Faucet"
)

func serveOauth(c *gin.Context) {
	providerName := c.Param("provider")
	provider, exists := oauthProviders[providerName]
	if !exists {
		c.JSON(http.StatusBadRequest, gin.H{"error": ErrUnsupportedProvider.Error()})
		return
	}

	type Request struct {
		Code  string `json:"code" binding:"required"`
		State string `json:"state" binding:"required"`
	}

	var body Request
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Exchange the `code` for an `access_token`
	token, err := OAuthExchangeCodeForToken(provider, body.Code, body.State)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Fetch the user's profile
	accountData, err := provider.FetchAccountData(token)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Update user record in Dynamo
	user, err := getUserWithProviderUniqueID(providerName, accountData.UniqueID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "fetching DynamoDB user: " + err.Error()})
		return
	}

	user.Accounts[providerName] = accountData

	err = saveUser(user)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "saving DynamoDB user: " + err.Error()})
		return
	}

	jwtToken := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"userID": user.ID,
		"nbf":    time.Date(2015, 10, 10, 12, 0, 0, 0, time.UTC).Unix(),
	})

	// Sign and get the complete encoded token as a string using the secret
	jwtTokenString, err := jwtToken.SignedString([]byte(env.JWTSecret))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "generating JWT: " + err.Error()})
		return
	}

	type Response struct {
		JWT string `json:"jwt"`
	}

	c.JSON(http.StatusOK, Response{jwtTokenString})
}

func serveVerifyAccount(c *gin.Context) {
	type Request struct {
		TargetAddr string `json:"targetAddr" binding:"required"`
	}

	var body Request
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	userID, err := getUserIDFromJWT(c)
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
		return
	}

	// This helps us keep the user locked while we wait to see if the message was successful.  If
	// we don't reach the point where we've submitted it, we go ahead and unlock the user right away.
	var successfullySubmittedMessage bool

	// Lock the user for the duration of this operation
	err = lockUser(userID, UserLock_Verifier)
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
		return
	}
	defer func() {
		if !successfullySubmittedMessage {
			unlockUser(userID, UserLock_Verifier)
		}
	}()

	user, err := getUserByID(userID)
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "user not found, have you authenticated?"})
		return
	}

	// Ensure that the user's account is old enough
	minAccountAge := time.Duration(env.VerifierMinAccountAgeDays) * 24 * time.Hour
	if !user.HasAccountOlderThan(minAccountAge) {
		c.JSON(http.StatusForbidden, gin.H{"error": ErrUserTooNew.Error()})
		return
	}

	// Ensure that the user hasn't asked for more allocation too recently
	if user.MostRecentAllocation.Add(30 * 24 * time.Hour).After(time.Now()) {
		c.JSON(http.StatusForbidden, gin.H{"error": ErrAllocatedTooRecently.Error()})
		return
	}

	// Ensure that the user is actually owed bytes
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	remaining, err := lotusCheckAccountRemainingBytes(ctx, body.TargetAddr)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	owed := big.Sub(env.MaxAllowanceBytes, remaining)
	if big.Cmp(owed, big.NewInt(0)) <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "you have verified data already, Greedy McRichbags"})
		return
	}

	// Allocate the bytes
	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	cid, err := lotusVerifyAccount(ctx, body.TargetAddr, owed.String())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	successfullySubmittedMessage = true

	// Respond to the HTTP request
	type Response struct {
		Cid string `json:"cid"`
	}
	c.JSON(http.StatusOK, Response{Cid: cid.String()})

	go func() {
		defer unlockUser(userID, UserLock_Verifier)

		// Determine whether the Filecoin message succeeded
		ctx, cancel = context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()

		ok, err := lotusWaitMessageResult(ctx, cid)
		if err != nil {
			// This is already logged in lotusWaitMessageResult
			return
		}
		user.VerifiedFilecoinAddress = body.TargetAddr
		if ok {
			user.MostRecentAllocation = time.Now()
		}
		err = saveUser(user)
		if err != nil {
			log.Println("error saving user:", err)
		}
	}()
}

func serveListVerifiers(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	verifiers, err := lotusListVerifiers(ctx)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, verifiers)
}

func serveListVerifiedClients(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	verifiedClients, err := lotusListVerifiedClients(ctx)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, verifiedClients)
}

func serveCheckAccountRemainingBytes(c *gin.Context) {
	targetAddr := c.Param("target_addr")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dcap, err := lotusCheckAccountRemainingBytes(ctx, targetAddr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if dcap.Int == nil {
		dcap = big.NewInt(0)
	}

	user, err := getUserByVerifiedFilecoinAddress(targetAddr)
	if err != nil {
		// no-op
	}

	type Response struct {
		RemainingBytes       string    `json:"remainingBytes"`
		MostRecentAllocation time.Time `json:"mostRecentAllocation"`
	}
	c.JSON(http.StatusOK, Response{dcap.String(), user.MostRecentAllocation})
}

func serveCheckVerifierRemainingBytes(c *gin.Context) {
	targetAddr := c.Param("target_addr")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dcap, err := lotusCheckVerifierRemainingBytes(ctx, targetAddr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, dcap)
}

func serveFaucet(c *gin.Context) {
	targetAddrStr := c.Param("target_addr")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	targetAddr, err := address.NewFromString(targetAddrStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	api, closer, err := lotusGetFullNodeAPI(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer closer()

	faucetAddr := env.FaucetAddr
	if faucetAddr == (address.Address{}) {
		faucetAddr, err = api.WalletDefaultAddress(ctx)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}

	userID, err := getUserIDFromJWT(c)
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
		return
	}

	// This helps us keep the user locked while we wait to see if the message was successful.  If
	// we don't reach the point where we've submitted it, we go ahead and unlock the user right away.
	var successfullySubmittedMessage bool

	// Lock the user for the duration of this operation
	err = lockUser(userID, UserLock_Faucet)
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
		return
	}
	defer func() {
		if !successfullySubmittedMessage {
			unlockUser(userID, UserLock_Faucet)
		}
	}()

	user, err := getUserByID(userID)
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "user not found, have you authenticated?"})
		return
	}

	// No account less than a week old is allowed any FIL
	if !user.HasAccountOlderThan(env.FaucetMinAccountAge) {
		c.JSON(http.StatusForbidden, gin.H{"error": ErrUserTooNew.Error()})
		return
	}

	// returns an ID address if its a miner address, otherwise an empty address
	minerAddr, err := lotusGetMinerAddr(ctx, targetAddr)
	if err != nil && errors.Cause(err) != ErrNotMiner {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	isMiner := !minerAddr.Empty()

	// ensure the non-miner/new miner hasn't already gotten their non-miner faucet tx
	if !isMiner && user.ReceivedNonMinerFaucetGrant {
		c.JSON(http.StatusForbidden, gin.H{"error": "non-miners can only use the faucet once"})
		return
	}

	// determine the grant size:
	//    for non-miners: the base rate
	//    for miners who are requesting for the first time: the base rate
	//    for miners who have already requested at least once:
	//    - Get the prevPower associated with the miner at last renewal
	//    - Get the currentPower of the miner
	//    - subtract prevPower - currentPower to get the powerDiff
	//    - grant size = powerDiff (in GiB) / 2
	var owed types.FIL
	if isMiner {
		if user.HasRequestedFromFaucetAsMiner() {
			if user.MostRecentMinerFaucetGrant.Add(env.FaucetRateLimit).After(time.Now()) {
				c.JSON(http.StatusForbidden, gin.H{"error": fmt.Sprintf("you may only use the faucet once every %v hours", env.FaucetRateLimit.Hours())})
				return
			}

			targetAddr = minerAddr

			receipt, err := api.StateSearchMsg(ctx, user.MostRecentMinerFaucetGrantCid)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}

			prevPower, err := lotusGetMinerPower(ctx, minerAddr, receipt.TipSet)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}

			currentPower, err := lotusGetMinerPower(ctx, minerAddr, types.EmptyTSK)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}

			powerDiff := types.BigSub(types.BigInt(prevPower.MinerPower.RawBytePower), types.BigInt(currentPower.MinerPower.RawBytePower))
			if types.BigCmp(powerDiff, types.NewInt(0)) > 0 {
				powerDiffGiB := types.BigDiv(powerDiff, types.NewInt(1073741824))
				owedInt := types.BigDiv(powerDiffGiB, types.NewInt(2))
				owed = types.FIL(owedInt)
			}
			// apply a lower bound
			if types.BigCmp(types.BigInt(owed), types.BigInt(env.FaucetMinGrant)) < 0 {
				owed = env.FaucetMinGrant
			}

		} else {
			owed = env.FaucetBaseRate
		}
	} else {
		owed = env.FaucetBaseRate
	}

	// owedFIL := types.FIL(types.BigDiv(owed, types.NewInt(build.FilecoinPrecision)))

	cid, err := lotusSendFIL(ctx, faucetAddr, targetAddr, owed)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	successfullySubmittedMessage = true

	// Respond to the HTTP request
	type Response struct {
		Cid  string `json:"cid"`
		Sent string `json:"sent"`
	}
	c.JSON(http.StatusOK, Response{
		Cid:  cid.String(),
		Sent: owed.String(),
	})

	go func() {
		defer unlockUser(userID, UserLock_Faucet)

		// Determine whether the Filecoin message succeeded
		ctx, cancel = context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()

		ok, err := lotusWaitMessageResult(ctx, cid)
		if err != nil {
			// This is already logged in lotusWaitMessageResult
			return
		} else if !ok {
			// Transaction failed
			log.Println("ERROR: faucet transaction failed")
			return
		}

		if !minerAddr.Empty() {
			user.MostRecentMinerFaucetGrant = time.Now()
			user.MostRecentMinerFaucetGrantCid = cid
		} else {
			user.ReceivedNonMinerFaucetGrant = true
		}

		err = saveUser(user)
		if err != nil {
			log.Println("error saving user:", err)
		}
	}()
}

func getUserIDFromJWT(c *gin.Context) (string, error) {
	authHeader := c.GetHeader("Authorization")
	if !strings.HasPrefix(authHeader, "Bearer ") {
		return "", errors.New("bad Authorization header")
	}

	jwtToken := strings.TrimSpace(authHeader[len("Bearer "):])

	token, err := jwt.Parse(jwtToken, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("Unexpected signing method: %v", token.Header["alg"])
		}
		return []byte(env.JWTSecret), nil
	})
	if err != nil {
		return "", err
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok || !token.Valid {
		return "", err
	}

	userID, ok := claims["userID"].(string)
	if !ok {
		return "", err
	}
	return userID, nil
}
