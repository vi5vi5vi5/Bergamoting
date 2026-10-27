package music

import "io"

// Decoder — декодер AAC → PCM S16LE 48 кГц стерео: на вход кадры ADTS, на выход PCM.
// Настоящий (Linux) запускает GStreamer колонки; в тестах подставляется поддельный.
type Decoder interface {
	io.Writer // кадры ADTS
	io.Reader // PCM
	// CloseWrite сообщает конец потока: декодер доработает остаток и закроет выход (io.EOF у Read).
	CloseWrite() error
	// Close прерывает работу и освобождает процесс.
	Close() error
}

// DecoderFactory создаёт новый декодер (по одному на сессию воспроизведения).
type DecoderFactory func() (Decoder, error)

// gstPipeline — конвейер декодирования внутри maind --gst-launch. Все элементы есть во вшитом GStreamer
// колонки (проверено: fdsrc, aacparse, avdec_aac, audioconvert, audioresample, fdsink). PCM уходит в fd 3, потому что
// stdout/stderr заняты журналом maind.
const gstPipeline = "fdsrc fd=0 ! aacparse ! avdec_aac ! audioconvert ! audioresample ! " +
	"audio/x-raw,rate=48000,channels=2,format=S16LE ! fdsink fd=3 sync=false"

// MaindPath — штатный maind колонки, в котором живёт GStreamer.
const MaindPath = "/system/vendor/quasar/maind"
