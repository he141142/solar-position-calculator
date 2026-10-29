// position-calculator nhận ForecastGenerated, tính phần thiếu/dư của khách hàng.
// Thiếu: lấy từ nhà máy của mình trước, phần còn lại phát DeficitDetected để mua trên sàn.
// Dư:    phát SurplusDetected để bán.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/he141142/solar-shared/bus"
	"github.com/he141142/solar-shared/domain"
	"github.com/he141142/solar-shared/zonecfg"
)

var (
	ddb  *dynamodb.Client
	eb   *eventbridge.Client
	ssmc *ssm.Client

	// Một loader (có cache) cho mỗi zone; zone lấy từ event, không cố định theo deployment.
	zoneLoaders   = map[string]*zonecfg.Loader{}
	zoneLoadersMu sync.Mutex

	table   = os.Getenv("TABLE_NAME")
	busName = os.Getenv("EVENT_BUS_NAME")
)

var (
	errAlreadyProcessed = errors.New("already processed")
	errUnknownZone      = errors.New("unknown zone")
)

func main() {
	awsCfg, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		panic(err)
	}
	ddb = dynamodb.NewFromConfig(awsCfg)
	eb = eventbridge.NewFromConfig(awsCfg)
	ssmc = ssm.NewFromConfig(awsCfg)
	lambda.Start(handle)
}

type settings struct{ threshold, maxBuy, minSell, plantCapacity float64 }

func loadSettings(ctx context.Context, zone string) (settings, error) {
	zoneLoadersMu.Lock()
	l, ok := zoneLoaders[zone]
	if !ok {
		l = zonecfg.New(ssmc, domain.ZoneParamPath(zone))
		zoneLoaders[zone] = l
	}
	zoneLoadersMu.Unlock()

	v, err := l.Get(ctx)
	if err != nil {
		return settings{}, err
	}
	if len(v) == 0 {
		return settings{}, fmt.Errorf("%w %q", errUnknownZone, zone)
	}
	var s settings
	var errs [4]error
	s.threshold, errs[0] = v.Float(domain.ParamDeficitThreshold)
	s.maxBuy, errs[1] = v.Float(domain.ParamMaxBuyPrice)
	s.minSell, errs[2] = v.Float(domain.ParamMinSellPrice)
	s.plantCapacity, errs[3] = v.Float(domain.ParamPlantCapacity)
	return s, errors.Join(errs[:]...)
}

func handle(ctx context.Context, ev events.EventBridgeEvent) error {
	var f domain.Forecast
	if err := json.Unmarshal(ev.Detail, &f); err != nil || f.CustomerID == "" || f.Interval == "" || f.Zone == "" {
		// Dữ liệu hỏng thì retry cũng vô ích -> log và bỏ qua.
		slog.Error("invalid forecast", "detail", string(ev.Detail), "err", err)
		return nil
	}
	s, err := loadSettings(ctx, f.Zone)
	if errors.Is(err, errUnknownZone) {
		slog.Error("forecast for unknown zone skipped", "zone", f.Zone, "customer", f.CustomerID)
		return nil
	}
	if err != nil {
		return err
	}

	key := map[string]ddbtypes.AttributeValue{
		"PK": str("CUSTOMER#" + f.CustomerID),
		"SK": str("POSITION#" + f.Interval),
	}
	// EventBridge giao ít nhất 1 lần -> claim vị thế trước để không xử lý trùng.
	if err := claim(ctx, key, f); err != nil {
		if errors.Is(err, errAlreadyProcessed) {
			slog.Info("duplicate forecast skipped", "customer", f.CustomerID, "interval", f.Interval)
			return nil
		}
		return err
	}

	net := round(f.ProductionKwh - f.ConsumptionKwh) // > 0 dư, < 0 thiếu
	var fromPlant float64
	var intent *domain.TradeIntent
	var detailType string

	switch {
	case -net > s.threshold:
		deficit := -net
		if fromPlant, err = allocateFromPlants(ctx, f.Zone, f.Interval, deficit, s.plantCapacity); err != nil {
			return release(ctx, key, err)
		}
		if rest := round(deficit - fromPlant); rest > s.threshold {
			intent, detailType = newIntent(f, "BUY", rest, s.maxBuy), domain.DeficitDetected
		}
	case net > s.threshold:
		intent, detailType = newIntent(f, "SELL", net, s.minSell), domain.SurplusDetected
	}

	if intent != nil {
		if err := bus.Publish(ctx, eb, busName, domain.SourcePosition, detailType, intent); err != nil {
			// Lưu ý POC: phần nhà máy đã cấp không được hoàn lại khi retry.
			return release(ctx, key, err)
		}
	}

	status := "BALANCED"
	if intent != nil {
		status = "SENT_TO_EXCHANGE"
	}
	_, err = ddb.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                &table,
		Key:                      key,
		UpdateExpression:         aws.String("SET netKwh = :net, fromPlantKwh = :plant, exchangeKwh = :ex, #st = :st"),
		ExpressionAttributeNames: map[string]string{"#st": "status"},
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":net":   num(net),
			":plant": num(fromPlant),
			":ex":    num(intentQty(intent)),
			":st":    str(status),
		},
	})
	slog.Info("position computed", "customer", f.CustomerID, "interval", f.Interval,
		"netKwh", net, "fromPlantKwh", fromPlant, "exchangeKwh", intentQty(intent))
	return err
}

func claim(ctx context.Context, key map[string]ddbtypes.AttributeValue, f domain.Forecast) error {
	item := map[string]ddbtypes.AttributeValue{
		"zone":           str(f.Zone),
		"productionKwh":  num(f.ProductionKwh),
		"consumptionKwh": num(f.ConsumptionKwh),
		"model":          str(f.Model),
		"status":         str("PROCESSING"),
		"ttl":            num(float64(time.Now().Add(30 * 24 * time.Hour).Unix())),
	}
	for k, v := range key {
		item[k] = v
	}
	_, err := ddb.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           &table,
		Item:                item,
		ConditionExpression: aws.String("attribute_not_exists(PK)"),
	})
	var ccf *ddbtypes.ConditionalCheckFailedException
	if errors.As(err, &ccf) {
		return errAlreadyProcessed
	}
	return err
}

// release xoá claim để lần retry của EventBridge có thể xử lý lại.
func release(ctx context.Context, key map[string]ddbtypes.AttributeValue, cause error) error {
	if _, err := ddb.DeleteItem(ctx, &dynamodb.DeleteItemInput{TableName: &table, Key: key}); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

// allocateFromPlants cấp tối đa `need` kWh từ pool nhà máy của khu vực trong khung giờ,
// dùng optimistic locking vì nhiều Lambda chạy song song cùng một khung.
func allocateFromPlants(ctx context.Context, zone, interval string, need, capacity float64) (float64, error) {
	key := map[string]ddbtypes.AttributeValue{
		"PK": str("ZONE#" + zone),
		"SK": str("PLANT_POOL#" + interval),
	}
	for range 5 {
		out, err := ddb.GetItem(ctx, &dynamodb.GetItemInput{TableName: &table, Key: key, ConsistentRead: aws.Bool(true)})
		if err != nil {
			return 0, err
		}

		used, cond := 0.0, "attribute_not_exists(usedKwh)"
		vals := map[string]ddbtypes.AttributeValue{}
		if v, ok := out.Item["usedKwh"].(*ddbtypes.AttributeValueMemberN); ok {
			if used, err = strconv.ParseFloat(v.Value, 64); err != nil {
				return 0, err
			}
			cond = "usedKwh = :prev"
			vals[":prev"] = v
		}

		alloc := round(math.Min(need, capacity-used))
		if alloc <= 0 {
			return 0, nil // nhà máy đã hết công suất cho khung này
		}
		vals[":new"] = num(round(used + alloc))
		vals[":cap"] = num(capacity)
		vals[":ttl"] = num(float64(time.Now().Add(7 * 24 * time.Hour).Unix()))

		_, err = ddb.UpdateItem(ctx, &dynamodb.UpdateItemInput{
			TableName:                 &table,
			Key:                       key,
			UpdateExpression:          aws.String("SET usedKwh = :new, capacityKwh = :cap, #ttl = :ttl"),
			ConditionExpression:       aws.String(cond),
			ExpressionAttributeNames:  map[string]string{"#ttl": "ttl"},
			ExpressionAttributeValues: vals,
		})
		var ccf *ddbtypes.ConditionalCheckFailedException
		if errors.As(err, &ccf) {
			continue // có Lambda khác vừa cấp, đọc lại
		}
		if err != nil {
			return 0, err
		}
		return alloc, nil
	}
	return 0, errors.New("plant pool contention: too many retries")
}

func newIntent(f domain.Forecast, side string, qty, limit float64) *domain.TradeIntent {
	return &domain.TradeIntent{
		IntentID:    fmt.Sprintf("%s#%s#%s", f.Zone, f.CustomerID, f.Interval),
		CustomerID:  f.CustomerID,
		Zone:        f.Zone,
		Interval:    f.Interval,
		Side:        side,
		QuantityKwh: qty,
		LimitPrice:  limit,
	}
}

func intentQty(i *domain.TradeIntent) float64 {
	if i == nil {
		return 0
	}
	return i.QuantityKwh
}

func round(f float64) float64 { return math.Round(f*1000) / 1000 }

func str(s string) ddbtypes.AttributeValue { return &ddbtypes.AttributeValueMemberS{Value: s} }

func num(f float64) ddbtypes.AttributeValue {
	return &ddbtypes.AttributeValueMemberN{Value: strconv.FormatFloat(f, 'f', -1, 64)}
}
