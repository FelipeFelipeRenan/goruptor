resource "aws_db_instance" "goruptor_rds" {
  identifier             = "goruptor-db"
  engine                 = "postgres"
  engine_version         = "15"
  instance_class         = "db.t3.micro"
  allocated_storage      = 20

  db_name                = "exchange"
  username               = "goruptor"
  password               = "admin123"

  skip_final_snapshot    = true
  publicly_accessible    = true
}

# ISSO AQUI É A CHAVE DO MISTÉRIO:
output "rds_endpoint" {
  value = aws_db_instance.goruptor_rds.endpoint
}