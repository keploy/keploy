import yaml
import os

#we are in ~/keploy

def yaml_as_python(val):
    """Convert YAML to dict"""
    try:
        return yaml.safe_load_all(val)
    except yaml.YAMLError as exc:
        return exc
subDir =os.listdir('./keploy')

if subDir[-1]=="testReports":
    subDir.pop()
oldMocks = []
newMocks = []

for dir in subDir:
    with open(os.path.join(os.getcwd()+"/keploy/",dir+"/mocks.yaml"),'r') as input_file:
        for item in (yaml_as_python(input_file)):
            oldMocks.append(item)

subDir=os.listdir('./keployTest990/keploy')
for dir in subDir:
    with open(os.path.join(os.getcwd()+"/keployTest990/keploy/",dir+"/mocks.yaml"),'r') as input_file:
        for item in (yaml_as_python(input_file)):
            newMocks.append(item)

if len(oldMocks) != len(newMocks):
    print("total mocks size different\n want :%v \n Got :%v ",len(oldMocks),len(newMocks))
else:
    for i in range(len(oldMocks)):
        # print("starting")
        if oldMocks[i]["spec"]["grpcReq"]["body"]==newMocks[i]["spec"]["grpcReq"]["body"]:
            print("values matched")
        else:
            print("mistach found in mock number :%v",i)
            print("Want :%s\nGot :%s",oldMocks[i]["spec"]["grpcReq"]["body"],newMocks[i]["spec"]["grpcReq"]["body"])
            break
        if oldMocks[i]["spec"]["grpcResp"]["body"]==newMocks[i]["spec"]["grpcResp"]["body"]:
            print("values matched")
        else:
            print("mismatch found in mock number :%v",i)
            print("value mismatched \nWant :%s\nGot :%s",oldMocks[i]["spec"]["grpcResp"]["body"],newMocks[i]["spec"]["grpcResp"]["body"])
            break

# for file in os.listdir("../../../../keploy/*"):
#     oldMocks = None
#     newMocks = None
#     with open('../../../../keploy/mocks/'+file,'r') as input_file:
#         print("-------------------------------new file started")
#         oldMocks = list(yaml_as_python(input_file))
#     with open('../../../../keployTest990/mocks/'+file,'r') as input_file:
#         newMocks = list(yaml_as_python(input_file))
#         # print("the oldMocks are ",oldMocks)
#     for valueOld,valueNew in zip(oldMocks,newMocks):
#          # print("printing the grpc timeouts value: \n",valueOld["spec"]["grpcReq"]["headers"]["ordinary_headers"]["grpc-timeout"],valueNew["spec"]["grpcReq"]["headers"]["ordinary_headers"]["grpc-timeout"])
#         if valueOld["spec"]["grpcReq"]["body"]==valueNew["spec"]["grpcReq"]["body"]:
#             print("values matched")
#         else:
#             print("mistach found in file :%s",file)
#             print("Want :%s\nGot :%s",valueOld["spec"]["grpcReq"]["body"],valueNew["spec"]["grpcReq"]["body"])
#             break
#         if valueOld["spec"]["grpcResp"]["body"]==valueNew["spec"]["grpcResp"]["body"]:
#             print("values matched")
#         else:
#             print("mismatch found in file :%s",file)
#             print("value mismatched \nWant :%s\nGot :%s",valueOld["spec"]["grpcResp"]["body"],valueNew["spec"]["grpcResp"]["body"])
#             break