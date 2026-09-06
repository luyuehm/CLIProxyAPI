package helper

// CSVUTF8BOM is the UTF-8 encoded Byte Order Mark byte sequence.
// Written at the head of a CSV byte stream, it lets Windows Excel / WPS
// detect the file as UTF-8, avoiding garbled Chinese content.
var CSVUTF8BOM = []byte{0xEF, 0xBB, 0xBF}
