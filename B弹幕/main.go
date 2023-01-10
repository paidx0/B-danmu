package main

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gorilla/websocket"
	"io"
	"net/http"
	"net/url"
	"time"
)

var startUrlFormat = "https://api.live.bilibili.com/xlive/web-room/v1/index/getDanmuInfo?id=%d"
var startInfo StartInfo

// 连接器
type BiliClient struct {
	RoomID     uint32
	HTTPClient *http.Client
	Conn       *websocket.Conn

	Ch chan PacketBody
}

type StartInfo struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	TTL     int    `json:"ttl"`
	Data    struct {
		Group            string  `json:"group"`
		BusinessID       int     `json:"business_id"`
		RefreshRowFactor float64 `json:"refresh_row_factor"`
		RefreshRate      int     `json:"refresh_rate"`
		MaxDelay         int     `json:"max_delay"`
		Token            string  `json:"token"`
		HostList         []struct {
			Host    string `json:"host"`
			Port    int    `json:"port"`
			WssPort int    `json:"wss_port"`
			WsPort  int    `json:"ws_port"`
		} `json:"host_list"`
	} `json:"data"`
}

func NewBiliClient(roomID uint32) *BiliClient {
	return &BiliClient{
		RoomID:     roomID,
		HTTPClient: &http.Client{},
		Conn:       nil,
		Ch:         make(chan PacketBody, 1024),
	}
}

// 返回 HostList
func (bc *BiliClient) GetHostList() []url.URL {
	req, err := http.NewRequest("GET", fmt.Sprintf(startUrlFormat, bc.RoomID), nil)
	if err != nil {
		panic(err)
	}

	resp, err := bc.HTTPClient.Do(req)
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()
	bts, err := io.ReadAll(resp.Body)
	if err != nil {
		panic(err)
	}
	err = json.Unmarshal(bts, &startInfo)
	if err != nil {
		panic(err)
	}

	hostList := make([]url.URL, 0, 3)
	for _, host := range startInfo.Data.HostList {
		u := url.URL{Scheme: "wss", Host: fmt.Sprintf("%s:%d", host.Host, host.WssPort), Path: "/sub"}
		hostList = append(hostList, u)
	}
	return hostList
}

// 与服务器建立连接
func (bc *BiliClient) Connect(urls []url.URL) {
	var websocketErr error = errors.New("websocket 连接错误！！！")
	// 有一个能连接成功就行
	for _, url := range urls {
		bc.Conn, _, websocketErr = websocket.DefaultDialer.Dial(url.String(), nil)
		if websocketErr != nil {
			Fail(fmt.Sprintf("WebSocket: %s 失败", url.String()))
			continue
		}
		Log(fmt.Sprintf("WebSocket: %s 连接成功", url.String()))
		break
	}
	if websocketErr != nil {
		panic(websocketErr)
	}

	type handShakeInfo struct {
		UID       uint8  `json:"uid"`
		Roomid    uint32 `json:"roomid"`
		Protover  uint8  `json:"protover"`
		Platform  string `json:"platform"`
		Clientver string `json:"clientver"`
		Type      uint8  `json:"type"`
		Key       string `json:"key"`
	}
	hsInfo := handShakeInfo{
		UID:       0,
		Roomid:    bc.RoomID,
		Protover:  2,
		Platform:  "web",
		Clientver: "1.10.2",
		Type:      2,
		Key:       startInfo.Data.Token,
	}
	b, err := json.Marshal(hsInfo)
	if err != nil {
		panic(err)
	}

	// 发送 JSON数据到服务器
	err = bc.SendPacket(0, 16, 0, 7, 0, b)
	if err != nil {
		panic(err)
	}

	Log(fmt.Sprintf("连接到房间 %d", bc.RoomID))
	Log("-------------------------------------")
}

func (bc *BiliClient) Disconnect() {
	err := bc.Conn.Close()
	if err != nil {
		Fail(err.Error())
	}
}

// 心跳测试
func (bc *BiliClient) HeartBeat() {
	for {
		err := bc.SendPacket(0, 16, 0, 2, 0, []byte(""))
		if err != nil {
			Fail(fmt.Sprintf("心跳出错，重试: %s", err))
			time.Sleep(5 * time.Second)
			continue
		}
		time.Sleep(20 * time.Second)
	}
}

// 数据包格式
type Packet struct {
	PacketLen  int
	HeaderLen  int
	Version    int
	Operation  int
	SequenceID int
	Body       []PacketBody
}

type PacketBody struct {
	Cmd     string                 `json:"cmd"`
	Data    map[string]interface{} `json:"data"`
	MsgSelf string                 `json:"msg_self"`
	Info    []interface{}          `json:"info"`
}

// 发 JSON数据包到服务器
func (bc *BiliClient) SendPacket(packetLen uint32, headerLen uint16, version uint16, operation uint32, sequenceID uint32, body []byte) error {
	if packetLen == 0 {
		packetLen = uint32(len(body) + 16)
	}
	header := new(bytes.Buffer)
	var data = []interface{}{
		packetLen,
		headerLen,
		version,
		operation,
		sequenceID,
	}
	for _, v := range data {
		err := binary.Write(header, binary.BigEndian, v)
		if err != nil {
			return err
		}
	}

	socketData := append(header.Bytes(), body...)
	err := bc.Conn.WriteMessage(websocket.TextMessage, socketData)
	return err
}

// 数据包解码
func (bc *BiliClient) Decode(blob []byte) (Packet, error) {
	// 截取包体
	result := Packet{
		PacketLen:  int(binary.BigEndian.Uint32(blob[0:4])),
		HeaderLen:  int(binary.BigEndian.Uint16(blob[4:6])),
		Version:    int(binary.BigEndian.Uint16(blob[6:8])),
		Operation:  int(binary.BigEndian.Uint32(blob[8:12])),
		SequenceID: int(binary.BigEndian.Uint32(blob[12:16])),
		Body:       make([]PacketBody, 0),
	}

	if result.Operation == 5 {
		offset := 0
		for offset < len(blob) {
			packetLen := int(binary.BigEndian.Uint32(blob[offset : offset+4]))
			if result.Version == 2 {
				data := blob[offset+result.HeaderLen : offset+packetLen]
				r, err := zlib.NewReader(bytes.NewReader(data))
				if err != nil {
					return Packet{}, err
				}
				defer r.Close()

				var newBlob []byte
				newBlob, err = io.ReadAll(r)
				if err != nil {
					return Packet{}, err
				}

				var child Packet
				child, err = bc.Decode(newBlob)
				if err != nil {
					return Packet{}, err
				}

				result.Body = append(result.Body, child.Body...)
			} else {
				var body PacketBody
				data := blob[offset+result.HeaderLen : offset+packetLen]
				if err := json.Unmarshal(data, &body); err != nil {
					return Packet{}, err
				}

				result.Body = append(result.Body, body)
			}
			offset += packetLen
		}
	}
	return result, nil
}

// 接收服务器信息
func (bc *BiliClient) ReceiveMessages() {
	for {
		_, message, err := bc.Conn.ReadMessage()
		if err != nil {
			Fail(fmt.Sprintf("客户端接收信息错误: %s", err))
		}

		packet, err := bc.Decode(message)
		if err != nil {
			Fail(fmt.Sprintf("数据包信息解码错误: %s", err))
		}

		// 信号量传递信息
		for _, body := range packet.Body {
			bc.Ch <- body
		}
	}
}

// 返回服务器的信息
func (bc *BiliClient) ParseMessages() {
	for {
		select {
		case msg := <-bc.Ch:
			switch msg.Cmd {
			case "COMBO_SEND": // 赠送礼物
				OK(fmt.Sprintf("[+] %s 送给 %s %b 个 %s", msg.Data["uname"].(string), msg.Data["r_uname"].(string), int(msg.Data["combo_num"].(float64)), msg.Data["gift_name"].(string)))
			case "DANMU_MSG": // 弹幕信息
				Warn(fmt.Sprintf("%s --> %s", msg.Info[2].([]interface{})[1].(string), msg.Info[1].(string)))
			case "USER_TOAST_MSG": // 续费舰长
				OK(fmt.Sprintf("[+] %s 续费舰长", msg.Data["user_info"].(map[string]string)["uname"]))
			case "GUARD_BUY": // 购买舰长
				OK(fmt.Sprintf("[+] %s 购买舰长", msg.Data["user_info"].(map[string]string)["uname"]))
			case "INTERACT_WORD": // 进入房间
				Log(fmt.Sprintf("[-] %s 进入了房间", msg.Data["uname"].(string)))
			case "SEND_GIFT": // 赠送礼物
				OK(fmt.Sprintf("[+] %s --> 投喂了 %b 个 %s", msg.Data["uname"], int(msg.Data["num"].(float64)), msg.Data["giftName"].(string)))
			case "NOTICE_MSG": // 直播间广播
				OK(fmt.Sprintf("[-] 广播信息 --> %s", msg.MsgSelf))
			case "LIVE":
				Log("[-] 直播开始！！！")
			case "PREPARING":
				Log("[-] 主播准备中！！！")
			default:
				continue
			}
		}
	}
}

func main() {
	biliClient := NewBiliClient(85213)
	hostList := biliClient.GetHostList()
	biliClient.Connect(hostList)
	defer biliClient.Disconnect()

	go biliClient.ReceiveMessages()
	go biliClient.ParseMessages()
	biliClient.HeartBeat()
}

/*
COMBO_SEND赠送礼物
SEND_GIFT赠送礼物
DANMU_MSG弹幕内容
GUARD_BUY购买舰长
USER_TOAST_MSG续费舰长
INTERACT_WORD进入房间
LIVE 直播开始
NOTICE_MSG 直播间广播
PREPARING 主播准备中

SUPER_CHAT_MESSAGE超级留言
SUPER_CHAT_MESSAGE_JPN超级留言-JP
SYS_MSG 系统消息

ROOM_REAL_TIME_MESSAGE_UPDATE 主播粉丝信息更新
HOT_RANK_CHANGED主播实时活动排名
ONLINE_RANK_COUNT高能榜数量更新
ONLINE_RANK_V2高能榜数据
ONLINE_RANK_TOP3用户进高能榜前三
*/

const (
	LogBule    = "\033[94m"
	OkGreen    = "\033[92m"
	WarnYellow = "\033[93m"
	FailRed    = "\033[91m"
	End        = "\033[0m"
)

func Log(str string) {
	fmt.Println(LogBule + str + End)
}
func OK(str string) {
	fmt.Println(OkGreen + str + End)
}
func Warn(str string) {
	fmt.Println(WarnYellow + str + End)
}
func Fail(str string) {
	fmt.Println(FailRed + str + End)
}
