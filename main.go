package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/PuerkitoBio/goquery"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/aws/aws-sdk-go/service/dynamodb/dynamodbattribute"
	"github.com/go-telegram-bot-api/telegram-bot-api"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	schedulePattern = `"data":{.*`
	prefixToRemove  = "\"data\":"
	suffixToRemove  = "}"
	yesMessage      = "Живлення подається. Якщо живлення відсутнє, це може означати що склалася аварійна ситуація. Інформацію уточнюйте у ДТЕК"
	noMessage       = "Живлення відсутнє. Слідкуйте за графіком на сайті ДТЕК"
	maybeMessage    = "Згідно із графіком, живлення може подаватися з вірогідністю 50/50. Слідкуйте за графіком на сайті ДТЕК"
)

type Option struct {
	OptionId    string `json:"id"`
	OptionValue string `json:"optionValue"`
}

type Event struct {
	UpdateId int64 `json:"update_id"`
	Message  struct {
		MessageId int64 `json:"message_id"`
		From      struct {
			Id           int64  `json:"id"`
			IsBot        bool   `json:"is_bot"`
			FirstName    string `json:"first_name"`
			LastName     string `json:"last_name"`
			Username     string `json:"username"`
			LanguageCode string `json:"language_code"`
			IsPremium    bool   `json:"is_premium"`
		} `json:"from"`
		Chat struct {
			Id        int64  `json:"id"`
			FirstName string `json:"first_name"`
			LastName  string `json:"last_name"`
			Username  string `json:"username"`
			Type      string `json:"type"`
		} `json:"chat"`
		Date     int64  `json:"date"`
		Text     string `json:"text"`
		Entities []struct {
			Offset int64  `json:"offset"`
			Length int64  `json:"length"`
			Type   string `json:"type"`
		} `json:"entities"`
	} `json:"message"`
}

var days = map[int]int{
	0: 6,
	1: 0,
	2: 1,
	3: 2,
	4: 3,
	5: 4,
	6: 5,
}

var tableName = "Options"
var uaLocation = time.FixedZone("UTC-7", 7200)

var telegramBot tgbotapi.BotAPI

func main() {
	lambda.Start(HandleRequest)
}

func HandleRequest(ctx context.Context, event Event) (events.APIGatewayProxyResponse, error) {
	Ok := events.APIGatewayProxyResponse{Body: "Successful Start", StatusCode: 200}
	err := initBot()
	if err != nil {
		return events.APIGatewayProxyResponse{}, err
	}

	err = processEvent(event)
	if err != nil {
		log.Println("#HandleRequest:", err)

		newMessage := tgbotapi.NewMessage(event.Message.Chat.Id, "Відзначаються складнощі у взаємодіїі із серверами ДТЕК. Треба почекати. Смерть окупантам :D")
		_, err = telegramBot.Send(newMessage)
		if err != nil {
			log.Println("#processEvent: error occurred during a message sending", err)
			return Ok, err
		}
	}
	return Ok, nil
}

func processEvent(event Event) error {
	log.Printf("#processEvent: Authorized on account %s", telegramBot.Self.UserName)

	message := event.Message
	switch message.Text {
	case "/start":
		var keyboard = tgbotapi.NewReplyKeyboard(
			tgbotapi.NewKeyboardButtonRow(
				tgbotapi.NewKeyboardButton("Надай мені статус електропостачання"),
			),
		)

		newMessage := tgbotapi.NewMessage(message.Chat.Id, message.Text)
		newMessage.ReplyMarkup = keyboard

		_, err := telegramBot.Send(newMessage)
		if err != nil {
			log.Println("#processEvent: error during sending a keyboard", err)
			return err
		}
		return nil
	case "Надай мені статус електропостачання":
		status, err := getElectricityData()
		if err != nil {
			log.Println("#processEvent: error occurred during the getting of electricity status", err)
			return err
		}

		log.Println(fmt.Sprintf("#processEvent: electricity status: %s", status))

		newMessage := tgbotapi.NewMessage(event.Message.Chat.Id, status)
		_, err = telegramBot.Send(newMessage)
		if err != nil {
			log.Println("#processEvent: error occurred during a message sending", err)
			return err
		}
		return nil
	default:
		newMessage := tgbotapi.NewMessage(event.Message.Chat.Id, "Не розумію команди. Будь ласка, користуйтеся наданою кнопкою")
		_, err := telegramBot.Send(newMessage)
		if err != nil {
			log.Println("#processEvent: error occurred during a message sending", err)
			return err
		}
		return nil
	}
}

func getElectricityData() (string, error) {
	optionExists := false
	scheduleStringOption, err := getOption("0")
	if err != nil {
		log.Println("#getElectricityData: option is not found. Setting up the option")
		scheduleStringOption = Option{OptionId: "0"}
	} else {
		optionExists = true
	}

	var scheduleString string

	if len(scheduleStringOption.OptionValue) == 0 {
		page, err := fetchSchedulePage()
		if err != nil {
			return "", err
		}

		scheduleString, err = extractScheduleString(page)
		if err != nil {
			return "", err
		}
	}

	groupSchedule, err := extractSchedule(scheduleString)
	if err != nil {
		return "", err
	}

	status, err := extractStatusByGroup(groupSchedule, 3)
	if err != nil {
		return "", err
	}

	scheduleStringOption.OptionValue = scheduleString
	log.Println("AAAAA", scheduleStringOption)

	if optionExists {
		err = updateOption(scheduleStringOption)
	} else {
		err = saveOption(scheduleStringOption)
	}

	if err != nil {
		log.Println("#getElectricityData: Error during setting string with groupSchedule")
		return "", err
	}

	return status, nil
}

func fetchSchedulePage() ([]byte, error) {
	pageResponse, err := http.Get("https://www.dtek-oem.com.ua/ua/shutdowns")
	if err != nil {
		log.Println("#fetchSchedulePage: Error during page fetch")
		return nil, err
	}
	page, err := io.ReadAll(pageResponse.Body)
	if err != nil {
		log.Println("#fetchSchedulePage: Error during page reading")
		return nil, err
	}
	return page, nil
}

func extractScheduleString(page []byte) (string, error) {
	if page == nil || len(page) == 0 {
		return "", errors.New("#extractScheduleString: page is absent or empty")
	}
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(page))
	if err != nil {
		log.Println("#extractScheduleString: Error during page reading")
		return "", err
	}
	var scheduleString string
	doc.Find("script").Each(func(_ int, script *goquery.Selection) {
		cont := script.Text()
		if strings.Contains(cont, "DisconSchedule") {
			scheduleString = cont
		}
	})
	if scheduleString == "" {
		return "", errors.New("#extractScheduleString: there's no schedule on the page")
	}
	matcher := regexp.MustCompile(schedulePattern)
	scheduleString = matcher.FindString(scheduleString)
	scheduleString = strings.TrimPrefix(scheduleString, prefixToRemove)
	scheduleString = strings.TrimSuffix(scheduleString, suffixToRemove)
	return scheduleString, nil
}

func extractSchedule(scheduleString string) (map[string]map[string]map[string]string, error) {
	var groupSchedule map[string]map[string]map[string]string
	err := json.Unmarshal([]byte(scheduleString), &groupSchedule)
	if err != nil {
		log.Println("#extractSchedule: Error during groupSchedule unmarshalling", err)
		return nil, err
	}
	return groupSchedule, nil
}

func extractStatusByGroup(groupSchedule map[string]map[string]map[string]string, group int) (string, error) {
	if group < 1 || group > 3 {
		return "", errors.New("#extractStatusByGroup: wrong group is passed. Should be from 1 inclusively to 3 inclusively")
	}

	var status string
	day, hour := getCurrentAdjustedDayAndHour()
	weekSchedule, ok := groupSchedule[strconv.Itoa(group)]
	if !ok {
		return "", errors.New(fmt.Sprintf("#extractStatusByGroup: there's no schedule for group %d", group))
	}

	daySchedule, ok := weekSchedule[day]
	if !ok {
		return "", errors.New(fmt.Sprintf("#extractStatusByGroup: there's no schedule for group %d, day %s", group, day))
	}

	status, ok = daySchedule[hour]
	if !ok {
		return "", errors.New(fmt.Sprintf("#extractStatusByGroup: there's no schedule for group %d, day %s, hour %s", group, day, hour))
	}

	if status == "yes" {
		return yesMessage, nil
	} else if status == "no" {
		return noMessage, nil
	} else {
		return maybeMessage, nil
	}
}

func getCurrentAdjustedDayAndHour() (string, string) {
	dateTime := time.Now().In(uaLocation)
	day := days[int(dateTime.Weekday())]
	hour, _, _ := dateTime.Clock()
	return strconv.Itoa(day + 1), strconv.Itoa(hour + 1)
}

func initBot() error {
	bot, err := tgbotapi.NewBotAPI(os.Getenv("BOT_TOKEN"))
	if err != nil {
		log.Print("HandleRequest: error occurred during a botApi creation")
		return err
	}
	telegramBot = *bot
	return nil
}

func getOption(optionId string) (Option, error) {
	sess := session.Must(session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable,
	}))

	svc := dynamodb.New(sess)

	result, err := svc.GetItem(&dynamodb.GetItemInput{
		TableName: aws.String(tableName),
		Key: map[string]*dynamodb.AttributeValue{
			"OptionId": {
				S: aws.String(optionId),
			},
		},
	})

	if result.Item == nil {
		return Option{}, errors.New("#getOption: fetched item is nil")
	}

	var option Option

	err = dynamodbattribute.UnmarshalMap(result.Item, &option)
	if err != nil {
		log.Println("#getOption error occurred during unmarshalling or of fetched item", err, option)
		return Option{}, nil
	}

	return option, nil
}

func saveOption(option Option) error {
	sess := session.Must(session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable,
	}))

	svc := dynamodb.New(sess)

	marshalledOption, err := dynamodbattribute.MarshalMap(option)
	if err != nil {
		log.Println("#saveOption: error occurred during marshalling for db put", err, option)
	}

	input := &dynamodb.PutItemInput{
		Item:      marshalledOption,
		TableName: aws.String(tableName),
	}

	_, err = svc.PutItem(input)
	if err != nil {
		log.Println("#saveOption: error occurred during put to db", err, option)
		return err
	}

	return nil
}

func updateOption(option Option) error {
	sess := session.Must(session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable,
	}))

	svc := dynamodb.New(sess)

	input := &dynamodb.UpdateItemInput{
		ExpressionAttributeValues: map[string]*dynamodb.AttributeValue{
			":optionValue": {
				S: aws.String(option.OptionValue),
			},
		},
		TableName: aws.String(tableName),
		Key: map[string]*dynamodb.AttributeValue{
			"id": {
				S: aws.String(option.OptionId),
			},
		},
		ReturnValues:     aws.String("UPDATED_NEW"),
		UpdateExpression: aws.String("set optionValue = :optionValue"),
	}

	_, err := svc.UpdateItem(input)
	if err != nil {
		log.Println("#updateOption: error occurred during update of db", err, option)
		return err
	}
	return nil
}

//
//func getElectricityOnOffHours(hourString string, status string, daySchedule map[string]string) (string, string, string, error) {
//	var maybe, yes, no string
//	hour, err := strconv.Atoi(hourString)
//	if err != nil {
//		log.Println("#getElectricityOnHour: error occurred during hour conversion")
//		return "", "", "", err
//	}
//	for i := hour; i < 23; i++ {
//		status := daySchedule[strconv.Itoa(hour)]
//		if status {
//
//		}
//	}
//}
