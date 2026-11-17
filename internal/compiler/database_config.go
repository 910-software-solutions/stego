package compiler

import "bytes"

func writeDatabaseConfiguration(buf *bytes.Buffer) {
	buf.WriteString("\tstegoStage = \"database.configure\"\n")
	buf.WriteString("\tdsn, err := stegoDatabaseURL()\n\tif err != nil {\n\t\tstegoStage = stegoDatabaseStage()\n\t\treturn err\n\t}\n")
}

// The process reads database secrets before it creates a pool or a listener.
// A refused setting names its variable in the stage, as the runtime names it
// (ST-SUP-RP14-LC05), and never its value.
const databaseConfigSource = `

// stegoDatabaseStage names the setting that stegoDatabaseURL refused: the file
// form when only it is set, else DATABASE_URL. It reads no value.
func stegoDatabaseStage() string {
 if os.Getenv("DATABASE_URL") == "" && os.Getenv("DATABASE_URL_FILE") != "" {
  return "service.start.setting.postgres-adapter.DATABASE_URL_FILE"
 }
 return "service.start.setting.postgres-adapter.DATABASE_URL"
}

func stegoDatabaseURL() (string,error) {
 value:=os.Getenv("DATABASE_URL")
 name:=os.Getenv("DATABASE_URL_FILE")
 invalid:=errors.New("invalid database configuration source")
 if (value=="")== (name==""){return "",invalid}
 if name==""{
  if len(value)>65536{return "",invalid}
  return value,nil
 }
 if len(name)>4096 || !filepath.IsAbs(name){return "",invalid}
 // Follow projected-volume symlinks. Check the opened descriptor so a path
 // replacement cannot bypass the file type and permission checks.
 file,err:=os.OpenFile(name,os.O_RDONLY|syscall.O_NONBLOCK,0)
 if err!=nil{return "",invalid}
 defer file.Close()
 info,err:=file.Stat()
 if err!=nil || !info.Mode().IsRegular() || info.Size()>65536 || info.Mode().Perm()&0137!=0{return "",invalid}
 data,err:=io.ReadAll(io.LimitReader(file,65537))
 if err!=nil || len(data)>65536{return "",invalid}
 value=string(data)
 if strings.HasSuffix(value,"\r\n"){value=value[:len(value)-2]}else{value=strings.TrimSuffix(value,"\n")}
 if value=="" || strings.ContainsAny(value,"\x00\r\n"){return "",invalid}
 return value,nil
}
`
