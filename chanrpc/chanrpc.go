package chanrpc

import (
	"errors"
	"fmt"
	"github.com/name5566/leaf/conf"
	"runtime"
)

// one server per goroutine (goroutine not safe)
// one client per goroutine (goroutine not safe)

//rpc服务器
type Server struct {
	functions map[interface{}]interface{} //id->func映射
	ChanCall  chan *CallInfo              //用于传递调用信息的管道
}

//调用信息
type CallInfo struct {
	f       interface{}   //函数
	args    []interface{} //参数
	chanRet chan *RetInfo //返回值管道,用于传输返回值,可能是同步返回值管道,也可能是异步返回值管道
	cb      interface{}   //回调
}

//返回信息
type RetInfo struct {
	ret interface{} //返回值
	err error       //错误
	cb  interface{} //回调
}

//rpc客户端
type Client struct {
	s               *Server       //rpc服务器引用
	chanSyncRet     chan *RetInfo //同步返回信息
	ChanAsynRet     chan *RetInfo //异步返回信息
	pendingAsynCall int           //待处理的异步调用计算器
}

//创建rpc服务器
func NewServer(l int) *Server {
	s := new(Server)                                //创建服务器
	s.functions = make(map[interface{}]interface{}) //创建id->func映射
	s.ChanCall = make(chan *CallInfo, l)            //创建用于传递调用信息的管道
	return s
}

//注册id->func的映射,必须在调用Open()和Go()之前调用
func (s *Server) Register(id interface{}, f interface{}) {
	switch f.(type) { //判断f的类型
	case func([]interface{}): //参数是切片,值任意,无返回值
	case func([]interface{}) interface{}: //参数是切片,值任意,返回一个任意值
	case func([]interface{}) []interface{}: //参数是切片,返回值也是切片,值均为任意
	default:
		panic(fmt.Sprintf("function id %v: definition of function is invalid", id)) //id对应的函数定义非法
	}

	if _, ok := s.functions[id]; ok { //判断映射是否存在
		panic(fmt.Sprintf("function id %v: already registered", id))
	}

	s.functions[id] = f //存储映射
}

//执行RPC调用
func (s *Server) Exec(ci *CallInfo) (err error) {
	//延迟处理异常
	defer func() {
		if r := recover(); r != nil {
			if conf.LenStackBuf > 0 {
				buf := make([]byte, conf.LenStackBuf)
				l := runtime.Stack(buf, false)
				err = fmt.Errorf("%v: %s", r, buf[:l])
			} else {
				err = fmt.Errorf("%v", r)
			}
			s.ret(ci, &RetInfo{err: err}) //返回一个错误
		}
	}()

	switch ci.f.(type) { //判断f类型
	case func([]interface{}): //无返回值
		ci.f.(func([]interface{}))(ci.args) //执行调用
		return s.ret(ci, &RetInfo{})        //返回值为空
	case func([]interface{}) interface{}: //一个返回值
		ret := ci.f.(func([]interface{}) interface{})(ci.args) //执行调用
		return s.ret(ci, &RetInfo{ret: ret})                   //一个返回值
	case func([]interface{}) []interface{}: //n个返回值
		ret := ci.f.(func([]interface{}) []interface{})(ci.args) //执行调用
		return s.ret(ci, &RetInfo{ret: ret})                     //多个返回值
	}

	panic("bug")
}

//rpc服务器调用自己
func (s *Server) Go(id interface{}, args ...interface{}) {
	f := s.functions[id] //根据id取得对应的f
	if f == nil {
		return
	}

	defer func() { //ChanCall有可能被关闭,导致传输调用消息时出现异常
		recover() //只是为了让程序不中断,不做多余的错误处理
	}()

	s.ChanCall <- &CallInfo{ //将调用消息通过管道传输到rpc服务器
		f:    f,
		args: args,
	}
}

//关闭rpc服务器
func (s *Server) Close() {
	close(s.ChanCall) //关闭用于传递调用信息的管道

	for ci := range s.ChanCall { //遍历所有未处理完的消息,返回错误消息
		s.ret(ci, &RetInfo{
			err: errors.New("chanrpc server closed"),
		})
	}
}

//打开一个rpc客户端
func (s *Server) Open(l int) *Client {
	c := new(Client)                       //创建一个rpc客户端
	c.s = s                                //保存rpc服务器引用
	c.chanSyncRet = make(chan *RetInfo, 1) //创建一个管道用于传输同步调用返回信息,同步调用的管道大小一定为1,因为调用以后就需要阻塞读取返回
	c.ChanAsynRet = make(chan *RetInfo, l) //创建一个管道用于传输异步调用返回信息,异步调用的管道大小不一定为1
	return c                               //返回rpc客户端
}

//返回
func (s *Server) ret(ci *CallInfo, ri *RetInfo) (err error) {
	if ci.chanRet == nil { //调用信息的返回值管道不存在
		return
	}

	defer func() { //延迟捕获异常
		if r := recover(); r != nil {
			err = r.(error)
		}
	}()

	ri.cb = ci.cb    //将调用信息中的回调函数保存到返回信息中(只有异步调用才有回调函数)
	ci.chanRet <- ri //将返回信息发送到返回值管道中
	return
}

//获取f
func (c *Client) f(id interface{}, n int) (f interface{}, err error) {
	f = c.s.functions[id] //根据id取得对应的f
	if f == nil {         //f未注册
		err = fmt.Errorf("function id %v: function not registered", id)
		return
	}

	var ok bool
	switch n { //根据n的值判断f类型是否正确
	case 0:
		_, ok = f.(func([]interface{})) //n为0,无返回值
	case 1:
		_, ok = f.(func([]interface{}) interface{}) //n为1,一个返回值
	case 2:
		_, ok = f.(func([]interface{}) []interface{}) //n为2,多个返回值
	default:
		panic("bug")
	}

	if !ok { //类型不匹配
		err = fmt.Errorf("function id %v: return type mismatch", id)
	}

	return
}

//发起调用
func (c *Client) call(ci *CallInfo, block bool) (err error) {
	defer func() { //延迟捕获异常
		if r := recover(); r != nil {
			err = r.(error)
		}
	}()

	if block { //阻塞
		c.s.ChanCall <- ci //将调用消息通过管道传输到rpc服务器,当管道满时阻塞
	} else { //非阻塞
		select {
		case c.s.ChanCall <- ci: //将调用消息通过管道传输到rpc服务器
		default: //当管道满时,返回管道已满错误
			err = errors.New("chanrpc channel full")
		}
	}

	return
}

//call0 call1 calln 可以将0 1 n记作0个返回值,1个返回值,n个返回值

//调用0
//适合参数是切片,值任意,无返回值
func (c *Client) Call0(id interface{}, args ...interface{}) error {
	f, err := c.f(id, 0) //获取f
	if err != nil {
		return err
	}

	err = c.call(&CallInfo{ //发起调用
		f:       f,
		args:    args,
		chanRet: c.chanSyncRet, //同步返回管道
	}, true)

	if err != nil {
		return err
	}

	ri := <-c.chanSyncRet //读取结果
	return ri.err         //返回错误字段,代表是否有错
}

//调用1
//适合参数是切片,值任意,返回值为一个任意值
func (c *Client) Call1(id interface{}, args ...interface{}) (interface{}, error) {
	f, err := c.f(id, 1) //获取f
	if err != nil {
		return nil, err
	}

	err = c.call(&CallInfo{ //发起调用
		f:       f,
		args:    args,
		chanRet: c.chanSyncRet, //同步返回管道
	}, true)

	if err != nil {
		return nil, err
	}

	ri := <-c.chanSyncRet //读取结果
	return ri.ret, ri.err //返回返回值字段和错误字段
}

//调用N
//适合参数是切片,返回值也是切片,值均为任意
func (c *Client) CallN(id interface{}, args ...interface{}) ([]interface{}, error) {
	f, err := c.f(id, 2) //获取f
	if err != nil {
		return nil, err
	}

	err = c.call(&CallInfo{ //发起调用
		f:       f,
		args:    args,
		chanRet: c.chanSyncRet, //同步返回管道
	}, true)

	if err != nil {
		return nil, err
	}

	ri := <-c.chanSyncRet                 //读取结果
	return ri.ret.([]interface{}), ri.err //返回返回值字段(先转化类型)和错误字段
}

//发起异步调用(内部的)
func (c *Client) asynCall(id interface{}, args []interface{}, cb interface{}, n int) error {
	f, err := c.f(id, n) //获得f
	if err != nil {
		return err
	}

	err = c.call(&CallInfo{ //发起调用
		f:       f,
		args:    args,
		chanRet: c.ChanAsynRet, //异步返回管道
		cb:      cb,
	}, false)

	if err != nil {
		return err
	}

	c.pendingAsynCall++ //增加待处理的异步调用计数器
	return nil
}

//发起异步调用(导出的)
//需要自己写c.Cb(<-c.ChanAsynRet)执行回调
func (c *Client) AsynCall(id interface{}, _args ...interface{}) { //_args最后一个是回调函数,前面的是rpc调用的参数
	if len(_args) < 1 { //检查是否提供了回调函数参数
		panic("callback function not found")
	}

	var args []interface{}
	if len(_args) > 1 {
		args = _args[:len(_args)-1] //取出rpc调用的参数
	}

	cb := _args[len(_args)-1] //取出回调函数
	switch cb.(type) {        //判断回调函数的类型
	case func(error): //只接收一个错误
		err := c.asynCall(id, args, cb, 0) //发起异步调用(内部)
		if err != nil {                    //调用失败,执行回调
			cb.(func(error))(err)
		}
	case func(interface{}, error): //接收一个返回值和一个错误
		err := c.asynCall(id, args, cb, 1) //发起异步调用(内部)
		if err != nil {                    //调用失败,执行回调
			cb.(func(interface{}, error))(nil, err)
		}
	case func([]interface{}, error): //接收多个返回值和一个错误
		err := c.asynCall(id, args, cb, 2) //发起异步调用(内部)
		if err != nil {                    //调用失败,执行回调
			cb.(func([]interface{}, error))(nil, err)
		}
	default:
		panic("definition of callback function is invalid")
	}
}

//执行回调
func (c *Client) Cb(ri *RetInfo) {
	switch ri.cb.(type) { //判断回调类型
	case func(error): //无返回值,只接收一个错误
		ri.cb.(func(error))(ri.err) //执行回调
	case func(interface{}, error): //一个返回值,一个错误
		ri.cb.(func(interface{}, error))(ri.ret, ri.err) //执行回调
	case func([]interface{}, error): //多个返回值,一个错误
		ri.cb.(func([]interface{}, error))(ri.ret.([]interface{}), ri.err) //执行回调
	default:
		panic("bug")
	}

	c.pendingAsynCall-- //减少计数器
}

//关闭rpc客户端
func (c *Client) Close() {
	for c.pendingAsynCall > 0 { //还存在未处理的异步调用,等待异步调用处理完毕,取出异步返回值,执行回调
		c.Cb(<-c.ChanAsynRet)
	}
}
