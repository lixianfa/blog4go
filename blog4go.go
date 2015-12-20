// Copyright 2015
// Author: huangjunwei@youmi.net

package blog4go

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"sync"
	"time"
)

// 各种日志结构接口
type LogWriter interface {
	// 关闭log writer的处理方法
	// 善后
	Close()

	// 用于内部写log的方法
	write(level Level, format string)
	writef(level Level, format string, args ...interface{})
}

var (
	// 好像buffer size 调大点benchmark效果更好
	// 默认使用内存页大小
	DefaultBufferSize = 4096

	ErrInvalidFormat = errors.New("Invalid format type.")
)

// 时间格式化的cache
type timeFormatCacheType struct {
	// 当前
	now time.Time
	// 当前日期
	date string
	// 当前时间格式化结果
	format []byte
	// 昨日日期
	date_yesterday string
}

// 用全局的timeCache好像比较好
// 实例的timeCache没那么好统一更新
var timeCache = timeFormatCacheType{}

// 装逼的logger
type FileLogWriter struct {
	level Level

	// log文件
	fileName string
	file     *os.File
	writer   *bufio.Writer

	// 互斥锁，用于互斥调用bufio
	lock *sync.Mutex

	// writer 关闭标识
	closed bool

	// logrotate
	// 互斥锁，用于互斥logrotate
	rotateLock *sync.Mutex
	// 按时间rotate
	timeRotated   bool
	timeRotateSig chan bool

	// size rotate按行数、大小rotate, 后缀 xxx.1, xxx.2
	sizeRotateSig chan bool

	// 按行rotate
	lineRotated  bool
	rotateLines  int
	currentLines int

	// 按大小rotate
	sizeRotated bool
	rotateSize  ByteSize
	currentSize ByteSize

	sizeRotateTimes int // 当前按size,line rotate次数

	// 记录每次log的size
	logSizeChan chan int

	// 日志等级是否带颜色输出
	colored bool

	// log hook
	hook Hook
}

// 包初始化函数
func init() {
	DefaultBufferSize = os.Getpagesize()
	timeCache.now = time.Now()
	timeCache.date = timeCache.now.Format(DateFormat)
	timeCache.format = []byte(timeCache.now.Format(PrefixTimeFormat))
	timeCache.date_yesterday = timeCache.now.Add(-24 * time.Hour).Format(DateFormat)
}

// 创建file writer
func NewFileLogWriter(fileName string) (fileWriter *FileLogWriter, err error) {
	fileWriter = new(FileLogWriter)
	fileWriter.fileName = fileName

	fileWriter.lock = new(sync.Mutex)
	fileWriter.closed = false

	// 日志轮询
	fileWriter.rotateLock = new(sync.Mutex)
	fileWriter.timeRotated = false
	fileWriter.timeRotateSig = make(chan bool)
	fileWriter.sizeRotateSig = make(chan bool)
	fileWriter.logSizeChan = make(chan int, 4096)

	fileWriter.lineRotated = false
	fileWriter.rotateSize = DefaultRotateSize
	fileWriter.currentSize = 0

	fileWriter.sizeRotated = false
	fileWriter.rotateLines = DefaultRotateLines
	fileWriter.currentLines = 0

	// 日志等级颜色输出
	fileWriter.colored = true

	// 打开文件描述符
	file, err := os.OpenFile(fileName, os.O_WRONLY|os.O_APPEND|os.O_CREATE, os.FileMode(0644))
	if nil != err {
		return nil, err
	}
	fileWriter.file = file
	fileWriter.writer = bufio.NewWriterSize(file, DefaultBufferSize)

	go fileWriter.daemon()

	return fileWriter, nil
}

func (self *FileLogWriter) SetLevel(level Level) *FileLogWriter {
	self.level = level
	return self
}

func (self *FileLogWriter) Level() Level {
	return self.level
}

func (self *FileLogWriter) SetTimeRotated(timeRotated bool) {
	self.timeRotated = timeRotated
}

func (self *FileLogWriter) RotateSize() ByteSize {
	return self.rotateSize
}

func (self *FileLogWriter) SetRotateSize(rotateSize ByteSize) {
	if rotateSize > ByteSize(0) {
		self.sizeRotated = true
		self.rotateSize = rotateSize
	} else {
		self.sizeRotated = false
	}
}

func (self *FileLogWriter) RotateLine() int {
	return self.rotateLines
}

func (self *FileLogWriter) SetRotateLines(rotateLines int) {
	if rotateLines > 0 {
		self.lineRotated = true
		self.rotateLines = rotateLines
	} else {
		self.lineRotated = false
	}
}

func (self *FileLogWriter) Colored() bool {
	return self.colored
}

func (self *FileLogWriter) SetColored(colored bool) {
	if colored == self.colored {
		return
	}

	self.colored = colored
	self.lock.Lock()
	defer self.lock.Unlock()

	initPrefix(colored)
}

func (self *FileLogWriter) SetHook(hook Hook) {
	self.hook = hook
}

func (self *FileLogWriter) Close() {
	self.lock.Lock()
	if self.closed {
		return
	}

	self.flush()
	self.file.Close()
	self.writer = nil
	self.closed = true
	self.lock.Unlock()
}

func (self *FileLogWriter) flush() {
	self.writer.Flush()
}

// 常驻goroutine, 更新格式化的时间
func (self *FileLogWriter) daemon() {
	t := time.Tick(1 * time.Second)

DaemonLoop:
	for {
		select {
		case <-t:
			if self.closed {
				break DaemonLoop
			}

			self.rotateLock.Lock()

			now := time.Now()
			timeCache.now = now
			timeCache.format = []byte(now.Format(PrefixTimeFormat))
			date := now.Format(DateFormat)

			if self.timeRotated && date != timeCache.date {
				// 需要rotate
				self.sizeRotateTimes = 0

				fileName := fmt.Sprintf("%s.%s", self.fileName, timeCache.date_yesterday)
				// 更新bufio文件
				self.lock.Lock()
				self.flush()
				os.Rename(self.fileName, fileName)
				err := self.resetFile()
				if nil == err {
					timeCache.date_yesterday = timeCache.date
					timeCache.date = now.Format(DateFormat)
				}
				self.lock.Unlock()
			}

			self.rotateLock.Unlock()
		// 统计log write size
		case size := <-self.logSizeChan:
			if self.closed {
				break DaemonLoop
			}

			if !self.sizeRotated && !self.lineRotated {
				continue
			}

			self.rotateLock.Lock()

			self.currentSize += ByteSize(size)
			self.currentLines++

			if (self.sizeRotated && self.currentSize > self.rotateSize) || (self.lineRotated && self.currentLines > self.rotateLines) {

				fileName := fmt.Sprintf("%s.%d", self.fileName, self.sizeRotateTimes+1)
				if self.timeRotated {
					fileName = fmt.Sprintf("%s.%s.%d", self.fileName, timeCache.date, self.sizeRotateTimes+1)

				}

				// 更新bufio文件
				self.lock.Lock()
				self.flush()
				os.Rename(self.fileName, fileName)

				err := self.resetFile()
				if nil == err {
					self.sizeRotateTimes++
					self.currentSize = 0
					self.currentLines = 0
				}
				self.lock.Unlock()
			}
			self.rotateLock.Unlock()
		}
	}
}

func (self *FileLogWriter) resetFile() (err error) {
	file, err := os.OpenFile(self.fileName, os.O_WRONLY|os.O_APPEND|os.O_CREATE, os.FileMode(0644))

	if nil != err {
		// 如果创建文件失败怎么做？
		// 重试？
		//file, err = os.OpenFile(self.fileName, os.O_WRONLY|os.O_APPEND|os.O_CREATE, os.FileMode(0644))
	}

	self.file.Close()
	self.file = file
	self.writer.Reset(file)

	return
}

func (self *FileLogWriter) write(level Level, format string) {
	if level < self.level {
		return
	}

	self.lock.Lock()
	defer func() {
		self.lock.Unlock()
		// logrotate
		if self.sizeRotated || self.lineRotated {
			self.logSizeChan <- len(timeCache.format) + len(level.Prefix()) + len(format) + 1
		}

		// 异步调用log hook
		if nil != self.hook {
			go func(level Level, format string) {
				self.hook.Fire(level, format)
			}(level, format)
		}
	}()

	if self.closed {
		return
	}

	self.writer.Write(timeCache.format)
	self.writer.WriteString(level.Prefix())

	pc, _, lineno, ok := runtime.Caller(2)
	if ok {
		self.writer.WriteString(fmt.Sprintf("%s:%d ", runtime.FuncForPC(pc).Name(), lineno))
	}

	self.writer.WriteString(format)
	self.writer.WriteByte(EOL)
}

// 格式化构造message
// 边解析边输出
// 使用 % 作占位符
func (self *FileLogWriter) writef(level Level, format string, args ...interface{}) {
	if level < self.level {
		return
	}

	self.lock.Lock()
	// 统计日志size
	var size int = 0

	defer func() {
		self.lock.Unlock()
		// logrotate
		if self.sizeRotated || self.lineRotated {
			self.logSizeChan <- size
		}

		// 异步调用log hook
		if nil != self.hook {
			go func(level Level, format string, args ...interface{}) {
				self.hook.Fire(level, fmt.Sprintf(format, args...))
			}(level, format, args...)
		}
	}()

	if self.closed {
		return
	}

	// 识别占位符标记
	var tag bool = false
	var tagPos int = 0
	// 转义字符标记
	var escape bool = false
	// 在处理的args 下标
	var n int = 0
	// 未输出的，第一个普通字符位置
	var last int = 0
	var s int = 0

	self.writer.Write(timeCache.format)
	self.writer.WriteString(level.Prefix())

	pc, _, lineno, ok := runtime.Caller(2)
	if ok {
		self.writer.WriteString(fmt.Sprintf("%s:%d ", runtime.FuncForPC(pc).Name(), lineno))
	}

	size += len(timeCache.format) + len(level.Prefix())

	for i, v := range format {
		if tag {
			switch v {
			// 类型检查/ 特殊字符处理
			// 占位符，有意义部分
			// 字符串
			// %s
			case 's':
				if escape {
					escape = false
				}

				if str, ok := args[n].(string); ok {
					s, _ = self.writer.WriteString(str)
					size += s
					n++
					last = i + 1
				} else {
					//return ErrInvalidFormat
				}
				tag = false
			// 整型
			// %d
			// 还没想好怎么兼容int, int32, int64
			case 'd':
				if escape {
					escape = false
				}

				s, _ = self.writer.WriteString(fmt.Sprintf(format[tagPos:i+1], args[n]))
				size += s
				n++
				last = i + 1
				tag = false
			// 浮点型
			// %.xf
			case 'f':
				if escape {
					escape = false
				}

				// 还没想到好的解决方案，先用fmt自带的
				s, _ = self.writer.WriteString(fmt.Sprintf(format[tagPos:i+1], args[n]))
				size += s
				n++
				last = i + 1
				tag = false
			// Value
			// {xxx:xxx}
			case 'v':
				if escape {
					escape = false
				}

				// 还没想到好的解决方案，先用fmt自带的
				s, _ = self.writer.WriteString(fmt.Sprintf(format[tagPos:i+1], args[n]))
				size += s
				n++
				last = i + 1
				tag = false
			// 布尔型
			// %t
			case 't':
				if escape {
					escape = false
				}

				if b, ok := args[n].(bool); ok {
					s, _ = self.writer.WriteString(strconv.FormatBool(b))
					size += s
					n++
					last = i + 1
				} else {
					//return ErrInvalidFormat
				}
				tag = false
			//转义符
			case ESCAPE:
				if escape {
					self.writer.WriteByte(ESCAPE)
					size += 1
				}
				escape = !escape
			//默认
			default:

			}

		} else {
			// 占位符，百分号
			if '%' == format[i] && !escape {
				tag = true
				tagPos = i
				s, _ = self.writer.WriteString(format[last:i])
				size += s
				escape = false
			}
		}
	}
	self.writer.WriteString(format[last:])
	self.writer.WriteByte(EOL)
	size += len(format[last:]) + 1
}

func (self *FileLogWriter) Debug(format string) {
	self.write(DEBUG, format)
}

func (self *FileLogWriter) Debugf(format string, args ...interface{}) {
	self.writef(DEBUG, format, args...)
}

func (self *FileLogWriter) Trace(format string) {
	self.write(TRACE, format)
}

func (self *FileLogWriter) Tracef(format string, args ...interface{}) {
	self.writef(TRACE, format, args...)
}

func (self *FileLogWriter) Info(format string) {
	self.write(INFO, format)
}

func (self *FileLogWriter) Infof(format string, args ...interface{}) {
	self.writef(INFO, format, args...)
}

func (self *FileLogWriter) Warn(format string) {
	self.write(WARNING, format)
}

func (self *FileLogWriter) Warnf(format string, args ...interface{}) {
	self.writef(WARNING, format, args...)
}

func (self *FileLogWriter) Error(format string) {
	self.write(ERROR, format)
}

func (self *FileLogWriter) Errorf(format string, args ...interface{}) {
	self.writef(ERROR, format, args...)
}

func (self *FileLogWriter) Critical(format string) {
	self.write(CRITICAL, format)
}

func (self *FileLogWriter) Criticalf(format string, args ...interface{}) {
	self.writef(CRITICAL, format, args...)
}
