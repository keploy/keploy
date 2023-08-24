import yaml
import os

# we are in ~/keploy/keploy

subDir =os.listdir('.')
if subDir[-1]=="testReports":
    subDir.pop()
urlfile = open('../apiUrl.txt', 'a')
for dir in subDir:
    for file in os.listdir(os.path.join(os.getcwd(),dir+"/tests")):
        with open(os.path.join(os.getcwd(),dir+"/tests/"+file),'r') as f:
            valuesYaml=yaml.load(f,Loader=yaml.FullLoader)
        url =valuesYaml['spec']['req']['url']
        urlfile.write(url+"\n")
urlfile.close()

