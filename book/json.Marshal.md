# json.Marshal
> 方法路径：go-go1.23.5/src/encoding/json/encode.go
- 入口函数： json.Marshal()
- newTypeEncoder方法：
  - 编码成JSON会根据不同的类型选择不同的函数，如果实现了MarshalJSON方法则直接执行该方法：
  - 方法调用路径：json.Marshal() -> marshal -> reflectValue -> valueEncoder -> typeEncoder -> newTypeEncoder
