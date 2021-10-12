package netw

import (
	"context"
	"errors"
	"game/global"
	"game/iface"
	"net"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/gorilla/websocket"
)

// Connection 链接
type Connection struct {
	// 当前连接的socket 套接字
	Conn *websocket.Conn
	// 当前连接的ID 也可以称作为SessionID，ID全局唯一
	ConnID int64
	// 消息管理MsgID和对应处理方法的消息管理模块
	MsgHandler iface.MsgHandle
	// 用户上次心跳时间
	HeartbeatTime time.Time
	// 告知该链接已经退出/停止的channel
	ctx context.Context

	cancel context.CancelFunc
	//缓冲管道，用于写goroutine之间的消息通信
	msgChan chan []byte
	sync.RWMutex
	// 当前连接的关闭状态
	isClosed bool
}

// NewConnection 创建连接的方法
func NewConnection(conn *websocket.Conn, connID int64, msgHandler iface.MsgHandle) *Connection {
	// 初始化Conn属性
	c := &Connection{
		Conn:          conn,
		ConnID:        connID,
		isClosed:      false,
		MsgHandler:    msgHandler,
		HeartbeatTime: time.Now(),
		msgChan:       make(chan []byte, global.Config.MaxMsgChanLen),
	}
	return c
}

// StartWriter 写消息Goroutine， 用户将数据发送给客户端
func (c *Connection) StartWriter() {
	zap.S().Debug("start [Writer Goroutine is running]")
	defer zap.S().Debug(c.RemoteAddr().String(), "[conn Writer exit!]")
	for {
		select {
		case data := <-c.msgChan:
			// 有数据要写给客户端
			if err := c.Conn.WriteMessage(global.Config.MessageType, data); err != nil {
				zap.S().Error("Send Data error:, ", err, " Conn Writer exit")
				return
			}
		case <-c.ctx.Done():
			return
		}
	}
}

// StartReader 读消息Goroutine，用于从客户端中读取数据
func (c *Connection) StartReader() {
	zap.S().Debug("start [Reader Goroutine is running]")
	defer zap.S().Debug(c.RemoteAddr().String(), "[conn Reader exit!]")
	// 创建拆包解包的对象
	for {
		select {
		case <-c.ctx.Done():
			return
		default:
			// 读取客户端的Msg
			_, msgData, err := c.Conn.ReadMessage()
			if err != nil {
				goto Wrr
			}
			// 拆包，得到msgID 和 data 放在msg中
			msg, err := global.Server.Packet().Unpack(msgData)
			if err != nil {
				zap.S().Error("unpack error ", err)
				goto Wrr
			}
			// 得到当前客户端请求的Request数据
			req := Request{
				conn: c,
				msg:  msg,
			}
			c.SetPingTime()
			if global.Config.WorkerPoolSize > 0 {
				// 已经启动工作池机制，将消息交给Worker处理
				c.MsgHandler.SendMsgToTaskQueue(&req)
			} else {
				// 从绑定好的消息和对应的处理方法中执行对应的Handle方法
				go c.MsgHandler.DoMsgHandler(&req)
			}
		}
	}
Wrr:
	c.Stop()
}

// 启动连接，让当前连接开始工作
func (c *Connection) Start() {
	c.ctx, c.cancel = context.WithCancel(context.Background())
	// 1 开启用户从客户端读取数据流程的Goroutine
	go c.StartReader()
	// 2 开启用于写回客户端数据流程的Goroutine
	go c.StartWriter()
	// 按照用户传递进来的创建连接时需要处理的业务，执行钩子方法
	global.Server.CallOnConnStart(c)
}

// 停止连接，结束当前连接状态M
func (c *Connection) Stop() {
	c.Lock()
	defer c.Unlock()
	// 如果用户注册了该链接的关闭回调业务，那么在此刻应该显示调用
	global.Server.CallOnConnStop(c)
	// 如果当前链接已经关闭
	if c.isClosed == true {
		return
	}

	zap.S().Debug("Conn Stop()...ConnID = ", c.ConnID)
	// 关闭Writer
	c.cancel()
	// 关闭socket链接
	c.Conn.Close()
	// 关闭该链接全部管道
	close(c.msgChan)
	// 设置标志位
	c.isClosed = true
	// 将链接从连接管理器中删除
	global.Server.GetConnMgr().Remove(c)
}

// 返回ctx，用于用户自定义的go程获取连接退出状态
func (c *Connection) Context() context.Context {
	return c.ctx
}

// 从当前连接获取原始的socket Conn
func (c *Connection) GetConnection() *websocket.Conn {
	return c.Conn
}

// 获取当前连接ID
func (c *Connection) GetConnID() int64 {
	return c.ConnID
}

// 获取远程客户端地址信息
func (c *Connection) RemoteAddr() net.Addr {
	return c.Conn.RemoteAddr()
}

// 直接将Message数据发送数据给远程的客户端
func (c *Connection) SendMsg(msgID uint32, data interface{}) error {
	c.RLock()
	defer c.RUnlock()
	if c.isClosed == true {
		return errors.New("connection closed when send msg")
	}
	// 将data封包，并且发送
	dp := global.Server.Packet()
	msg, err := dp.Pack(NewMsgPackage(msgID, data))
	if err != nil {
		zap.S().Error("pack error msg ID = ", msgID)
		return errors.New("pack error msg ")
	}
	// 写回客户端
	c.msgChan <- msg
	return nil
}

// 设置心跳时间
func (c *Connection) SetPingTime() {
	c.Lock()
	defer c.Unlock()
	c.HeartbeatTime = time.Now()
}

/**
心跳超时
*/
func (c *Connection) IsHeartbeatTimeout() (timeout bool) {
	c.RLock()
	defer c.RUnlock()
	if time.Now().Before(c.HeartbeatTime.Add(time.Second * 30)) {
		timeout = true
	}
	return
}
