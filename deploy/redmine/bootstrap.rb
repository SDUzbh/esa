admin = User.find_by!(login: 'admin')
admin.password = ENV.fetch('REDMINE_ADMIN_PASSWORD')
admin.password_confirmation = admin.password
admin.must_change_passwd = false
admin.language = 'en'
admin.save!

Setting.default_language = 'en'
Setting.host_name = '127.0.0.1:8080'
Setting.protocol = 'http'

puts "Administrator initialized; Redmine #{Redmine::VERSION.to_s}"
