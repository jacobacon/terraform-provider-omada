# Adopt an existing switch and set its name.
resource "omada_switch" "office" {
  mac  = "8C-86-DD-10-50-CA"
  name = "Office Switch"
}
