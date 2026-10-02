# Render: vhs docs/img/explain.tape  (cần vhs + ttyd + ffmpeg)
Output docs/img/explain.gif
Set Shell "bash"
Set FontSize 15
Set Width 1240
Set Height 660
Set Theme "Catppuccin Mocha"
Set TypingSpeed 25ms
Set Padding 16

Hide
Type "go build -o /tmp/minidb ./cmd/minidb && rm -f /tmp/explain-demo.db* && clear" Enter
Sleep 4s
Type "/tmp/minidb -q -db /tmp/explain-demo.db" Enter
Sleep 1s
Show

Type "CREATE TABLE dim (kind INT, name TEXT, PRIMARY KEY (kind));" Enter
Sleep 400ms
Type "CREATE TABLE ev (id INT, kind INT, city TEXT, PRIMARY KEY (id));" Enter
Sleep 400ms
Type "INSERT INTO dim VALUES (3,'car'),(6,'metro'),(9,'van');" Enter
Sleep 400ms
Type "INSERT INTO ev VALUES (1,3,'hanoi'),(2,6,'hue'),(3,9,'danang'),(4,3,'hcm'),(5,6,'cantho');" Enter
Sleep 400ms
Type "ANALYZE dim; ANALYZE ev;" Enter
Sleep 600ms
Type "EXPLAIN SELECT ev.city, dim.name FROM ev JOIN dim ON ev.kind = dim.kind WHERE dim.kind < 5 ORDER BY ev.city;" Enter
Sleep 5s
