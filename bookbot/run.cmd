



SET BINARY=bin\bookbot_windows_amd64.exe

SET BOOKSPATH=~\Documents\Libros

SET DBPATH=~\book.db

SET PWD=




call %BINARY% --dir=%BOOKSPATH% --db=%DBPATH% --pwd=%PWD%